package main

// The 3D room: a shared multiplayer space per channel, one sub-room per area
// (office, garden, ...). The server's part is deliberately small and splits the
// same way p2p.go does:
//
//   - presence, movement, chat bubbles, object physics and voice signalling are
//     RELAYED and never stored. The server checks who may speak, rebuilds every
//     payload field by field, and fans it out to the channel's members. When the
//     last socket closes, the room is empty and nothing remembers it.
//   - the two shared boards (tasks, drawing) and the voice-note wall are
//     PERSISTED, because they are the things that should still be there
//     tomorrow. Plain bearer-authed HTTP, membership-checked per request.
//
// The one field the server does not inspect is rtc.payload: it is SDP/ICE
// between two peers, opaque here for exactly the reasons p2p.go lays out.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Field caps. A capped string is truncated rather than refused: these are
// cosmetic fields streamed at 10 Hz, and a notice per oversized frame would be
// its own flood.
const (
	roomMaxRoom  = 24
	roomMaxAnim  = 16
	roomMaxLook  = 48
	roomMaxCarry = 64
	roomMaxID    = 64
	roomMaxKind  = 24
	roomMaxTo    = 64
	roomCoordCap = 1000.0

	roomBoardMax = 512 << 10 // serialized board document
	roomNoteMax  = 5 << 20   // one voice note's audio bytes
	roomNoteKeep = 50        // newest notes kept per (channel, room)
)

// The sub-rooms a voice note can be left in. Single source: the wall endpoints
// validate against this list.
var roomAreas = []string{"office", "garden", "kitchen", "lounge", "beach"}

// Every room a person can stand in, walkable areas plus the generated worlds.
// A message carries the id of the one it was said in, so the stream can mark
// the line [office] or [dunes]; anything not on this list is refused rather
// than stamped, because the mark is rendered as-is.
var roomIDs = []string{"office", "garden", "kitchen", "lounge", "beach",
	"sea", "dunes", "camp", "space", "terminal"}

func validRoom(id string) bool { return slices.Contains(roomIDs, id) }

var roomBoardKinds = []string{"tasks", "drawing"}

// The audio containers MediaRecorder actually produces, plus wav for the odd
// client that encodes itself.
var roomNoteTypes = []string{"audio/webm", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/wav"}

func capRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func clampCoord(v float64) float64 {
	if v < -roomCoordCap {
		return -roomCoordCap
	}
	if v > roomCoordCap {
		return roomCoordCap
	}
	return v
}

// ---- slash commands ----

func registerRoomCommands(r *Registry) {
	room := &Command{
		Name: "room0", Usage: "/room0 [invite <nick|#channel>|quiet]",
		Desc:  "open the 3D room, ask someone in, or keep your room lines out of the stream",
		Scope: "channel", MinRole: "member",
		Args: []Arg{{Name: "action", Type: "text"}, {Name: "who", Type: "nick"}},
		Handler: func(c *Ctx) error {
			switch strings.ToLower(c.Arg(0)) {
			case "invite":
				return cmdRoomInvite(c)
			case "quiet":
				// The flag itself lives in the client: the server only stamps
				// the bit a sender puts on a message. This frame asks the one
				// connection that typed it to flip its own switch.
				c.Conn.Send(Frame{T: "room.quiet", D: map[string]any{"channel": c.Channel.ID}})
				return nil
			case "":
				// The cmdUpdate/FocusBlob shape: one frame to the connection
				// that typed it, because you asked HERE.
				c.Conn.Send(Frame{T: "room.open", D: map[string]any{"channel": c.Channel.ID}})
				return nil
			default:
				return fmt.Errorf("no such room0 action: %s", c.Arg(0))
			}
		},
	}
	r.AddBuiltin(room)
	// The same door by its other name, the way /standup aliases /checkin.
	d3 := *room
	d3.Name, d3.Usage, d3.Desc = "3d", "/3d", "alias of /room0"
	r.AddBuiltin(&d3)
}

// cmdRoomInvite posts the one event the invite is made of. It is written once
// and never updated: whether its join button still works is read off who is
// standing in the room right now, which every client already tracks.
func cmdRoomInvite(c *Ctx) error {
	// Where the inviter is standing, so the invite names a room to spawn in.
	room := c.Srv.roomSeatOf(c.User.ID, c.Channel.ID)
	if room == "" {
		room = "office"
	}
	who := strings.TrimSpace(c.Arg(1))

	switch {
	case who == "" || who == "#"+c.Channel.Name:
		return c.Srv.Post(c.Channel, &Event{Kind: "room_invite", Actor: c.User.Nick,
			actorID: c.User.ID, Body: room}, "")

	case strings.HasPrefix(who, "#"):
		ch, err := c.Srv.St.ChannelByName(strings.TrimPrefix(who, "#"))
		if err != nil {
			return fmt.Errorf("no channel %s", who)
		}
		if _, err := c.Srv.requireMember(ch.ID, c.User.ID); err != nil {
			return err
		}
		return c.Srv.Post(ch, &Event{Kind: "room_invite", Actor: c.User.Nick,
			actorID: c.User.ID, Body: room}, "")

	default:
		u, err := c.Srv.St.UserByNick(who)
		if err != nil {
			return fmt.Errorf("no such user: %s", who)
		}
		dm, err := c.Srv.DMChannel(c.User, u)
		if err != nil {
			return err
		}
		// The invite names its room in the body and its person in meta, so a
		// client can say "invited you" rather than "invited the channel".
		//
		// `channel` is the one the inviter is standing in, and it is what the
		// join button opens. Without it the DM would be the only channel the
		// row knows about, and a DM has no room to walk into.
		return c.Srv.Post(dm, &Event{Kind: "room_invite", Actor: c.User.Nick,
			actorID: c.User.ID, Body: room,
			Meta: map[string]any{
				"target": u.ID, "target_nick": u.Nick, "channel": c.Channel.ID,
			}}, "")
	}
}

// ---- realtime relay ----

// roomMsg is every field any room.* frame can carry — one struct rather than
// six, the p2pSignal precedent. Unknown client fields die here: the outgoing
// payload is rebuilt field by field, never forwarded.
type roomMsg struct {
	Channel string `json:"channel"`
	Type    string `json:"type"`
	Room    string `json:"room"`

	// avatar state (join / move / announce)
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	H      float64 `json:"h"`
	Anim   string  `json:"anim"`
	Look   string  `json:"look"`
	Carry  string  `json:"carry"`
	Moving bool    `json:"moving"`
	Voice  bool    `json:"voice"`

	// object
	ID     string  `json:"id"`
	VX     float64 `json:"vx"`
	VY     float64 `json:"vy"`
	Holder string  `json:"holder"`

	// rtc: opaque SDP/ICE between two peers, never parsed here (see p2p.go)
	To      string          `json:"to"`
	Payload json.RawMessage `json:"payload"`

	// emote (also rtc's kind)
	Kind string `json:"kind"`
}

// handleRoomFrame relays room.* traffic. Returns false if the frame is not one
// of ours, so the main dispatcher can carry on.
func (s *Server) handleRoomFrame(c *Conn, typ string, raw json.RawMessage) bool {
	switch typ {
	case "room.join", "room.send", "room.leave":
	default:
		return false
	}

	var d roomMsg
	if err := json.Unmarshal(raw, &d); err != nil {
		s.errTo(c, "", errors.New("malformed room frame"))
		return true
	}

	if typ == "room.leave" {
		// Only a room this conn actually stood in gets a leave — anything else
		// would let a non-member broadcast departures into rooms at will.
		if s.roomUnmark(c, d.Channel) {
			s.roomBroadcast(d.Channel, map[string]any{
				"channel": d.Channel, "type": "leave", "user_id": c.user.ID})
		}
		return true
	}

	if s.throttled(c, "room", d.Channel) {
		return true
	}
	// Membership is re-read from the store on every frame, exactly like the p2p
	// relay: kicking someone out of the channel stops their avatar mid-stride.
	if _, err := s.requireMember(d.Channel, c.user.ID); err != nil {
		s.errTo(c, d.Channel, err)
		return true
	}

	if typ == "room.join" {
		s.roomSit(c, d.Channel, capRunes(d.Room, roomMaxRoom), false)
		s.roomBroadcast(d.Channel, map[string]any{
			"channel": d.Channel, "type": "join",
			"user_id": c.user.ID, "name": c.user.Nick,
			"room": capRunes(d.Room, roomMaxRoom),
			"x":    clampCoord(d.X), "y": clampCoord(d.Y), "h": clampCoord(d.H),
			"anim": capRunes(d.Anim, roomMaxAnim), "look": capRunes(d.Look, roomMaxLook)})
		return true
	}

	// room.send — only after a room.join on this same conn.
	s.roomMu.Lock()
	joined := s.roomJoined[c][d.Channel] != nil
	s.roomMu.Unlock()
	if !joined {
		s.errTo(c, d.Channel, errors.New("join the room first"))
		return true
	}

	// Rebuild the payload FIELD BY FIELD per type. A pass-through relay would
	// carry any extra keys a client invented — the p2p.go covert-channel
	// argument, verbatim. name always comes from the server-side user record.
	out := map[string]any{"channel": d.Channel, "user_id": c.user.ID}
	switch d.Type {
	case "move", "announce":
		// announce is a move that asks the room to answer with their own state;
		// on the wire out they are the same thing.
		out["type"] = "move"
		out["name"] = c.user.Nick
		out["room"] = capRunes(d.Room, roomMaxRoom)
		out["x"], out["y"], out["h"] = clampCoord(d.X), clampCoord(d.Y), clampCoord(d.H)
		out["anim"] = capRunes(d.Anim, roomMaxAnim)
		out["moving"], out["voice"] = d.Moving, d.Voice
		out["look"] = capRunes(d.Look, roomMaxLook)
		if d.Carry != "" {
			out["carry"] = capRunes(d.Carry, roomMaxCarry)
		}
		// Where this person is standing and whether they are on voice is what
		// the channel's live strip is drawn from, so the seat follows them.
		s.roomSit(c, d.Channel, capRunes(d.Room, roomMaxRoom), d.Voice)
	case "object":
		if d.ID == "" || len(d.ID) > roomMaxID {
			s.errTo(c, d.Channel, errors.New("object needs an id"))
			return true
		}
		holder := capRunes(d.Holder, roomMaxCarry)
		// "me" is the one holder a client may claim, and it is rewritten to the
		// sender's id so nobody puts an object in someone else's hands.
		if holder == "me" {
			holder = c.user.ID
		}
		out["type"], out["id"] = "object", d.ID
		out["room"] = capRunes(d.Room, roomMaxRoom)
		out["x"], out["y"], out["h"] = clampCoord(d.X), clampCoord(d.Y), clampCoord(d.H)
		out["vx"], out["vy"] = clampCoord(d.VX), clampCoord(d.VY)
		out["holder"] = holder
	case "rtc":
		out["type"] = "rtc"
		out["to"] = capRunes(d.To, roomMaxTo)
		out["kind"] = capRunes(d.Kind, roomMaxKind)
		out["payload"] = d.Payload
	case "emote":
		out["type"], out["name"] = "emote", c.user.Nick
		out["kind"] = capRunes(d.Kind, roomMaxKind)
	default:
		s.errTo(c, d.Channel, fmt.Errorf("unknown room event %q", d.Type))
		return true
	}
	s.roomBroadcast(d.Channel, out)
	return true
}

// roomBroadcast fans a room.event out to every member of the channel — sender
// included, because clients filter their own echo by user_id.
func (s *Server) roomBroadcast(cid string, d map[string]any) {
	f := Frame{T: "room.event", D: d}
	for _, uid := range s.St.MemberIDs(cid) {
		s.SendToUser(uid, f)
	}
}

// roomSeat is where one connection is standing. Presence only: it dies with
// the socket, like everything else in the relay.
type roomSeat struct {
	Room  string
	Voice bool
}

// roomOccupant is one person in a channel's room, as the channel state frame
// reports them. One entry per person however many tabs they have open.
type roomOccupant struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Room   string `json:"room"`
	Voice  bool   `json:"voice"`
}

// roomSit records (or updates) where a conn is standing, and stamps the
// moment a channel's room went from empty to occupied.
func (s *Server) roomSit(c *Conn, cid, room string, voice bool) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	if s.roomJoined[c] == nil {
		s.roomJoined[c] = map[string]*roomSeat{}
	}
	if s.roomJoined[c][cid] == nil {
		if s.roomSince[cid] == "" {
			s.roomSince[cid] = now()
		}
		s.roomJoined[c][cid] = &roomSeat{}
	}
	seat := s.roomJoined[c][cid]
	if room != "" {
		seat.Room = room
	}
	seat.Voice = voice
}

// roomOccupants lists who is in cid's room right now.
//
// ponytail: a scan of every seated conn per call, which is nothing while a
// server holds hundreds of sockets. Index by channel if that stops being true.
func (s *Server) roomOccupants(cid string) ([]roomOccupant, string) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	var out []roomOccupant
	seen := map[string]bool{}
	for c, rooms := range s.roomJoined {
		seat := rooms[cid]
		if seat == nil || seen[c.user.ID] {
			continue
		}
		seen[c.user.ID] = true
		out = append(out, roomOccupant{UserID: c.user.ID, Name: c.user.Nick,
			Room: seat.Room, Voice: seat.Voice})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, s.roomSince[cid]
}

// roomSeatOf answers where a user is standing, for the invite that names a
// room to spawn in. Empty when they are not in that channel's room at all.
func (s *Server) roomSeatOf(uid, cid string) string {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	for c, rooms := range s.roomJoined {
		if c.user.ID == uid && rooms[cid] != nil {
			return rooms[cid].Room
		}
	}
	return ""
}

// roomForget drops a seat and, when it was the last one, forgets when the room
// started. Caller holds nothing; this takes roomMu itself.
func (s *Server) roomForget(cid string) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	for _, rooms := range s.roomJoined {
		if rooms[cid] != nil {
			return // somebody is still standing there
		}
	}
	delete(s.roomSince, cid)
}

// roomUnmark forgets that conn stood in cid's room; reports whether it did.
func (s *Server) roomUnmark(c *Conn, cid string) bool {
	s.roomMu.Lock()
	if s.roomJoined[c][cid] == nil {
		s.roomMu.Unlock()
		return false
	}
	delete(s.roomJoined[c], cid)
	s.roomMu.Unlock()
	s.roomForget(cid)
	return true
}

// roomDisconnect is the socket-close half of room.leave: whatever rooms the
// conn was in, everyone else sees them walk out.
func (s *Server) roomDisconnect(c *Conn) {
	s.roomMu.Lock()
	joined := s.roomJoined[c]
	delete(s.roomJoined, c)
	s.roomMu.Unlock()
	for cid := range joined {
		s.roomForget(cid)
		s.roomBroadcast(cid, map[string]any{
			"channel": cid, "type": "leave", "user_id": c.user.ID})
	}
}

// ---- store: boards ----

type RoomBoard struct {
	Kind    string
	Data    json.RawMessage
	Updated string
}

func (b *RoomBoard) view() map[string]any {
	return map[string]any{"kind": b.Kind, "data": b.Data, "updated_at": b.Updated}
}

// RoomBoardFor returns the stored board, or an empty one — a board that was
// never saved is an empty board, not an error.
func (s *Store) RoomBoardFor(cid, kind string) (*RoomBoard, error) {
	b := &RoomBoard{Kind: kind, Data: json.RawMessage("{}")}
	var data string
	err := s.db.QueryRow(`SELECT data, updated_at FROM room_boards WHERE channel_id=? AND kind=?`,
		cid, kind).Scan(&data, &b.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	b.Data = json.RawMessage(data)
	return b, nil
}

func (s *Store) SetRoomBoard(cid, kind string, data []byte) (*RoomBoard, error) {
	at := now()
	_, err := s.db.Exec(`INSERT INTO room_boards(channel_id,kind,data,updated_at) VALUES(?,?,?,?)
	  ON CONFLICT(channel_id,kind) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		cid, kind, string(data), at)
	return &RoomBoard{Kind: kind, Data: data, Updated: at}, err
}

// ---- store: voice notes ----

type RoomNote struct {
	ID          string
	Channel     string
	Room        string
	UserID      string
	AuthorName  string
	DurationMS  int64
	Size        int64
	ContentType string
	Created     string
}

func (n *RoomNote) view() map[string]any {
	return map[string]any{
		"id": n.ID, "room": n.Room, "duration_ms": n.DurationMS,
		"created_at": n.Created, "author_id": n.UserID, "author_name": n.AuthorName,
		"audio_url": "/api/room/notes/" + n.ID + "/audio",
	}
}

func (s *Store) AddRoomNote(cid, room string, u *User, durMs, size int64, ctype string) (*RoomNote, error) {
	n := &RoomNote{ID: newNoteID(), Channel: cid, Room: room, UserID: u.ID, AuthorName: u.Nick,
		DurationMS: durMs, Size: size, ContentType: ctype, Created: now()}
	_, err := s.db.Exec(`INSERT INTO room_notes(id,channel_id,room,user_id,duration_ms,size_bytes,content_type,created_at)
	  VALUES(?,?,?,?,?,?,?,?)`, n.ID, cid, room, u.ID, durMs, size, ctype, n.Created)
	return n, err
}

// newNoteID sorts by creation time: the clock in nanoseconds, then randomness.
// It is what breaks created_at's one-second ties on the wall, in both
// databases (SQLite's rowid did that once; PostgreSQL has none).
func newNoteID() string { return fmt.Sprintf("vn_%016x%s", time.Now().UnixNano(), id("")[1:9]) }

func noteKey(nid string) string { return "roomnotes/" + nid }

// RoomNotes lists a wall newest-first; the id breaks one-second ties.
func (s *Store) RoomNotes(cid, room string, limit int) ([]RoomNote, error) {
	rows, err := s.db.Query(`SELECT n.id,n.channel_id,n.room,n.user_id,u.nick,n.duration_ms,n.size_bytes,n.content_type,n.created_at
	  FROM room_notes n JOIN users u ON u.id=n.user_id
	  WHERE n.channel_id=? AND n.room=? ORDER BY n.created_at DESC, n.id DESC LIMIT ?`, cid, room, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoomNote{}
	for rows.Next() {
		var n RoomNote
		if err := rows.Scan(&n.ID, &n.Channel, &n.Room, &n.UserID, &n.AuthorName,
			&n.DurationMS, &n.Size, &n.ContentType, &n.Created); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) RoomNoteByID(nid string) (*RoomNote, error) {
	var n RoomNote
	err := s.db.QueryRow(`SELECT n.id,n.channel_id,n.room,n.user_id,u.nick,n.duration_ms,n.size_bytes,n.content_type,n.created_at
	  FROM room_notes n JOIN users u ON u.id=n.user_id WHERE n.id=?`, nid).
		Scan(&n.ID, &n.Channel, &n.Room, &n.UserID, &n.AuthorName,
			&n.DurationMS, &n.Size, &n.ContentType, &n.Created)
	if err != nil {
		return nil, ErrNotFound
	}
	return &n, nil
}

func (s *Store) DeleteRoomNote(nid string) error {
	_, err := s.db.Exec(`DELETE FROM room_notes WHERE id=?`, nid)
	return err
}

// ReapRoomNotes deletes everything past the newest `keep` on one wall and
// returns the reaped ids, so the caller can remove the files too.
func (s *Store) ReapRoomNotes(cid, room string, keep int) []string {
	rows, err := s.db.Query(`SELECT id FROM room_notes WHERE channel_id=? AND room=?
	  ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, cid, room, math.MaxInt32, keep)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var nid string
		if rows.Scan(&nid) == nil {
			ids = append(ids, nid)
		}
	}
	rows.Close()
	for _, nid := range ids {
		s.db.Exec(`DELETE FROM room_notes WHERE id=?`, nid)
	}
	return ids
}

// ---- HTTP: boards + voice notes ----

// roomRoutes wires the persistent half. The audio bytes live in s.Files at
// roomnotes/<id>, beside uploads/.
func (s *Server) roomRoutes(mux *http.ServeMux) {
	// The auth + membership preamble every room endpoint shares. cid comes from
	// wherever the endpoint keeps it (query or form), so it is an argument.
	member := func(w http.ResponseWriter, r *http.Request, cid string) *User {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return nil
		}
		if _, ok := s.St.MemberRole(cid, u.ID); !ok {
			fail(w, 403, "not a member of that channel")
			return nil
		}
		return u
	}

	mux.HandleFunc("GET /api/room/board", func(w http.ResponseWriter, r *http.Request) {
		cid, kind := r.URL.Query().Get("channel"), r.URL.Query().Get("kind")
		if !slices.Contains(roomBoardKinds, kind) {
			fail(w, 400, "kind must be one of: "+strings.Join(roomBoardKinds, ", "))
			return
		}
		if member(w, r, cid) == nil {
			return
		}
		b, err := s.St.RoomBoardFor(cid, kind)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"board": b.view()})
	})

	mux.HandleFunc("PUT /api/room/board", func(w http.ResponseWriter, r *http.Request) {
		cid, kind := r.URL.Query().Get("channel"), r.URL.Query().Get("kind")
		if !slices.Contains(roomBoardKinds, kind) {
			fail(w, 400, "kind must be one of: "+strings.Join(roomBoardKinds, ", "))
			return
		}
		if member(w, r, cid) == nil {
			return
		}
		var d struct {
			Data json.RawMessage `json:"data"`
		}
		// The read cap leaves headroom over the board cap so a document just
		// over the line gets the honest 413 below, not a truncation parse error.
		if err := json.NewDecoder(io.LimitReader(r.Body, roomBoardMax+(64<<10))).Decode(&d); err != nil {
			fail(w, 400, "body must be JSON: {\"data\":{...}}")
			return
		}
		data := bytes.TrimSpace(d.Data)
		if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
			fail(w, 400, "data must be a JSON object")
			return
		}
		if len(data) > roomBoardMax {
			fail(w, 413, fmt.Sprintf("board too large — max %d bytes", roomBoardMax))
			return
		}
		b, err := s.St.SetRoomBoard(cid, kind, data)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"board": b.view()})
	})

	mux.HandleFunc("GET /api/room/notes", func(w http.ResponseWriter, r *http.Request) {
		cid, room := r.URL.Query().Get("channel"), r.URL.Query().Get("room")
		if !slices.Contains(roomAreas, room) {
			fail(w, 400, "room must be one of: "+strings.Join(roomAreas, ", "))
			return
		}
		if member(w, r, cid) == nil {
			return
		}
		notes, err := s.St.RoomNotes(cid, room, roomNoteKeep)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		views := make([]map[string]any, 0, len(notes))
		for i := range notes {
			views = append(views, notes[i].view())
		}
		writeJSON(w, 200, map[string]any{"voice_notes": views})
	})

	mux.HandleFunc("POST /api/room/notes", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(roomNoteMax + (1 << 20)); err != nil {
			fail(w, 400, "multipart form required")
			return
		}
		cid, room := r.FormValue("channel"), r.FormValue("room")
		if !slices.Contains(roomAreas, room) {
			fail(w, 400, "room must be one of: "+strings.Join(roomAreas, ", "))
			return
		}
		u := member(w, r, cid)
		if u == nil {
			return
		}
		durMs, _ := strconv.ParseInt(r.FormValue("duration_ms"), 10, 64)
		if durMs < 1 || durMs > 120000 {
			fail(w, 400, "duration_ms must be 1..120000")
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			fail(w, 400, "file is required")
			return
		}
		defer f.Close()
		if hdr.Size > roomNoteMax {
			fail(w, 413, fmt.Sprintf("audio too large — max %d bytes", roomNoteMax))
			return
		}
		ctype, _, _ := strings.Cut(hdr.Header.Get("Content-Type"), ";")
		ctype = strings.TrimSpace(ctype)
		if !slices.Contains(roomNoteTypes, ctype) {
			fail(w, 400, "content type must be one of: "+strings.Join(roomNoteTypes, ", "))
			return
		}
		n, err := s.St.AddRoomNote(cid, room, u, durMs, hdr.Size, ctype)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if err := s.Files.write(noteKey(n.ID), 0, f, hdr.Size); err != nil {
			s.St.DeleteRoomNote(n.ID)
			s.Files.remove(noteKey(n.ID))
			fail(w, 500, err.Error())
			return
		}
		// Keep the wall at its cap: rows AND files, or the disk grows forever
		// under rows nothing lists any more.
		for _, old := range s.St.ReapRoomNotes(cid, room, roomNoteKeep) {
			s.Files.remove(noteKey(old))
		}
		s.roomBroadcast(cid, map[string]any{
			"channel": cid, "type": "vnote", "user_id": u.ID, "op": "new", "note": n.view()})
		writeJSON(w, 201, map[string]any{"voice_note": n.view()})
	})

	mux.HandleFunc("DELETE /api/room/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		n, err := s.St.RoomNoteByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such voice note")
			return
		}
		role, _ := s.St.MemberRole(n.Channel, u.ID)
		if n.UserID != u.ID && role != "op" && u.Role != "admin" {
			fail(w, 403, "only the author or an op can delete a voice note")
			return
		}
		if err := s.St.DeleteRoomNote(n.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.Files.remove(noteKey(n.ID))
		s.roomBroadcast(n.Channel, map[string]any{
			"channel": n.Channel, "type": "vnote", "user_id": u.ID, "op": "del",
			"id": n.ID, "room": n.Room})
		w.WriteHeader(204)
	})

	// Auth exactly the way /api/file is: authUser, which also accepts ?token=
	// for the <audio> tag that cannot set a header.
	mux.HandleFunc("GET /api/room/notes/{id}/audio", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		n, err := s.St.RoomNoteByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such voice note")
			return
		}
		if _, ok := s.St.MemberRole(n.Channel, u.ID); !ok {
			fail(w, 403, "not a member of that channel")
			return
		}
		w.Header().Set("Content-Type", n.ContentType)
		w.Header().Set("Content-Disposition", "inline")
		s.Files.serve(w, r, noteKey(n.ID))
	})
}
