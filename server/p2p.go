package main

// Peer-to-peer file transfer in DMs (spec F7), server side.
//
// The server is a signalling relay and nothing else. WebRTC needs the two peers
// to swap an SDP offer/answer and a trickle of ICE candidates before a data
// channel can open; those blobs have to travel over *some* already-authenticated
// channel, and the chat socket is the one both peers are already holding. So
// that is all this file does: check who is allowed to talk to whom, then hand
// the opaque blob over.
//
// What the server deliberately does NOT do:
//
//   - it never sees a single byte of the file. The bytes go over the data
//     channel, direct or via TURN; neither path comes back here.
//   - it never parses the SDP or the candidates. They are strings with a length
//     cap. Parsing them would mean the server understands (and could rewrite)
//     the transport, which is exactly the property F7 is claiming not to have.
//   - it never relays outside the DM. P2P is DM-only per docs/plans/gliph.md
//     assumption 9: a DM is a two-person authenticated room, so "the peer" is
//     unambiguous and there is nobody to leak an offer to. In a 40-person
//     channel there is no such thing as "the other member", and a stray offer
//     would be a de-anonymising IP-address broadcast to 39 people.
//
// The whole handshake, in order:
//
//	sender    p2p.begin  {id, name, size, sha256}   "I want to send you this"
//	recipient p2p.accept {id} | p2p.reject {id}     explicit consent, before bytes
//	sender    p2p.offer  {id, sdp}                  \
//	recipient p2p.answer {id, sdp}                   > opaque to the server
//	both      p2p.ice    {id, candidate, mid}       /
//	either    p2p.cancel {id, reason}               abort at any point
//
// ponytail: no server-side transfer state at all — no table, no in-memory map of
// live transfers. The transfer id is minted by the sender and only ever compared
// by the two clients; the server just copies it through. That means the server
// cannot enforce handshake ordering (a client could send an offer before a
// begin), which is fine: both ends of the conversation are the only parties that
// care, and the peer's client drops a frame for a transfer it never accepted.
// If we ever want server-visible transfer status (an admin audit trail, say),
// that is when state gets added here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Size caps. A frame over the cap is dropped with a notice rather than being
// truncated, because a truncated SDP would fail later in a much more confusing
// place. The numbers are generous: a full SDP with bundled candidates runs a few
// KB, an ICE candidate line a couple of hundred bytes.
const (
	maxSDPLen       = 16 << 10
	maxCandidateLen = 1024
	maxFileNameLen  = 256
	maxTransferID   = 64
	maxReasonLen    = 200
)

// p2pSignal is every field any p2p.* frame can carry. One struct rather than
// five because they share the routing header (channel + transfer id) and the
// per-type validation is three lines each.
type p2pSignal struct {
	Channel string `json:"channel"`
	ID      string `json:"id"` // transfer id, minted by the sender

	// p2p.begin: what the recipient is being asked to accept. The server stores
	// none of it and checks only that it is sane enough to render in a prompt.
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"` // hex digest the receiver verifies the blob against

	// p2p.offer / p2p.answer: opaque SDP. Never parsed here.
	SDP string `json:"sdp"`

	// p2p.ice: one trickled candidate plus the media id it belongs to.
	Candidate string `json:"candidate"`
	Mid       string `json:"mid"`

	// p2p.reject / p2p.cancel: a human-readable reason for the other end.
	Reason string `json:"reason"`
}

// handleP2PFrame handles WebRTC signalling for DM file transfers. Returns false
// if the frame is not one of ours, so the main dispatcher can carry on.
func (s *Server) handleP2PFrame(c *Conn, typ string, raw json.RawMessage) bool {
	switch typ {
	case "p2p.begin", "p2p.accept", "p2p.reject", "p2p.offer", "p2p.answer", "p2p.ice", "p2p.cancel":
	default:
		return false
	}

	var d p2pSignal
	if err := json.Unmarshal(raw, &d); err != nil {
		s.errTo(c, "", errors.New("malformed p2p frame"))
		return true
	}
	if d.ID == "" || len(d.ID) > maxTransferID {
		s.errTo(c, d.Channel, errors.New("p2p frame needs a transfer id"))
		return true
	}

	// Authorisation, in the order that fails cheapest first: the channel has to
	// exist, it has to be a DM, and the sender has to be in it. Membership is
	// re-read from the store on every frame rather than cached on the Conn, so
	// removing someone from a DM stops their signalling immediately.
	ch, err := s.St.ChannelByID(d.Channel)
	if err != nil {
		s.errTo(c, d.Channel, errors.New("unknown channel"))
		return true
	}
	if _, err := s.requireMember(ch.ID, c.user.ID); err != nil {
		s.errTo(c, d.Channel, err)
		return true
	}
	if !ch.IsDM {
		s.errTo(c, d.Channel, errors.New("peer-to-peer transfer only works in a direct message"))
		return true
	}

	if err := validateP2P(typ, &d); err != nil {
		s.errTo(c, d.Channel, err)
		return true
	}

	// Rebuild the outgoing payload field by field instead of forwarding what
	// arrived. A pass-through relay would happily carry any extra keys a client
	// invented, which is a covert channel between two clients that the server
	// claims to be mediating. The peer sees exactly the fields documented above,
	// plus who sent them.
	out := map[string]any{
		"channel": ch.ID, "id": d.ID,
		"from": c.user.Nick, "from_user": c.user.ID,
	}
	switch typ {
	case "p2p.begin":
		out["name"], out["size"], out["sha256"] = d.Name, d.Size, d.Sha256
	case "p2p.offer", "p2p.answer":
		out["sdp"] = d.SDP
	case "p2p.ice":
		out["candidate"], out["mid"] = d.Candidate, d.Mid
	case "p2p.reject", "p2p.cancel":
		out["reason"] = d.Reason
	}

	// Relay to the *other* member's connections — every one of them, because we
	// do not know which device the human is sitting at. The peer's client
	// ignores frames for transfers it is not driving.
	peers := 0
	for _, uid := range s.St.MemberIDs(ch.ID) {
		if uid == c.user.ID {
			continue
		}
		peers++
		s.SendToUser(uid, Frame{T: typ, D: out})
	}
	if peers == 0 {
		// A DM with one member is a DM with yourself, or the other side left.
		// Say so rather than letting the sender's transfer hang forever.
		s.errTo(c, ch.ID, errors.New("nobody on the other end of this DM"))
	}
	return true
}

// validateP2P checks the fields that matter for one frame type. It is a
// well-formedness check, not an interpretation: the SDP and the candidates are
// only ever measured, never read.
func validateP2P(typ string, d *p2pSignal) error {
	switch typ {
	case "p2p.begin":
		// The recipient is about to see this in an accept prompt, so it has to
		// be renderable. Control characters and newlines would let a sender
		// forge extra lines in that prompt.
		name := strings.TrimSpace(d.Name)
		if name == "" || len(name) > maxFileNameLen || strings.ContainsAny(name, "\n\r\t") {
			return errors.New("p2p.begin needs a plain filename")
		}
		d.Name = name
		if d.Size <= 0 {
			return errors.New("p2p.begin needs a positive file size")
		}
		// SHA-256 hex, nothing else: the receiver compares it against what it
		// actually assembled, so a garbled digest must fail here rather than
		// turn into a mystery "corrupt transfer" at the end.
		if len(d.Sha256) != 64 || strings.TrimLeft(strings.ToLower(d.Sha256), "0123456789abcdef") != "" {
			return errors.New("p2p.begin needs a sha-256 hex digest")
		}
	case "p2p.offer", "p2p.answer":
		if d.SDP == "" || len(d.SDP) > maxSDPLen {
			return fmt.Errorf("%s needs an sdp of at most %d bytes", typ, maxSDPLen)
		}
	case "p2p.ice":
		if d.Candidate == "" || len(d.Candidate) > maxCandidateLen || len(d.Mid) > 32 {
			return errors.New("malformed ice candidate")
		}
	case "p2p.reject", "p2p.cancel":
		if len(d.Reason) > maxReasonLen {
			d.Reason = d.Reason[:maxReasonLen]
		}
	}
	return nil
}
