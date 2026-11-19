package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Frame struct {
	T string `json:"t"`
	D any    `json:"d,omitempty"`
}

// Conn is one connected user client (a browser, desktop or mobile app).
type Conn struct {
	srv  *Server
	ws   *websocket.Conn
	user *User
	// device is this socket's E2EE device id, set by the device.hello frame.
	// Sealed channel keys are addressed to it and never leave the server for
	// any other device.
	device string
	// client is what the socket said it is (`/ws?client=desktop/0.1.54+300aae9`),
	// for /status and nothing else.
	client string
	out    chan Frame
	once   sync.Once
}

func (c *Conn) Send(f Frame) {
	select {
	case c.out <- f:
	default: // ponytail: slow client drops frames rather than stalling the hub; it re-syncs by cursor on reconnect.
	}
}

func (c *Conn) close() { c.once.Do(func() { close(c.out) }) }

type Server struct {
	St  *Store
	Reg *Registry

	Blobs *BlobRegistry

	mu      sync.RWMutex
	conns   map[string][]*Conn // user id -> connections
	plugins map[string]*PluginConn
	// offered records the actions the last blob document sent to a user
	// actually rendered, so an event naming anything else is refused.
	offered map[offeredKey]map[string]string
	// in-flight blob calls waiting on a plugin's reply
	blobCalls map[string]chan blobReply
	// which conns have joined which channel's 3D room — presence only, relayed
	// and never persisted. Its own mutex, not mu: broadcasting from under mu
	// would re-enter SendToUser's RLock. See room.go.
	roomMu sync.Mutex
	// pluginMu orders a plugin's channel membership against what it posts:
	// /invite, /kick, its subscribe list, and every result or say. One writer
	// process (see Append), so this is the whole serialisation.
	pluginMu sync.Mutex
	// plugRegMu orders a plugin's connection handoff, its cleanup and its
	// register frames, so one connection's cleanup never removes what the
	// next one registered.
	plugRegMu  sync.Mutex
	roomJoined map[*Conn]map[string]*roomSeat
	// When each channel's room went from empty to occupied, so the live strip
	// can say how long it has been running. Cleared when the last seat goes.
	roomSince map[string]string

	// Channels whose member lists owe their online members a refresh, flushed
	// together after presenceDelay. See presence.go.
	presMu    sync.Mutex
	presDirty map[string]bool

	// Push is nil-safe: a server with no relay configured simply never wakes
	// anyone, and everything else works. See push.go.
	Push *pushHub

	// ICE tells clients where to find STUN and TURN for peer-to-peer transfers.
	// Empty is fine: direct transfers still work, relayed ones do not.
	ICE ICEConfig

	// Mail sends the digests a sleeping user gets when push could not reach
	// them. noMail unless the operator configured credentials, and never
	// carries message content — see email.go.
	Mail Mailer

	// DownloadDir holds published desktop releases (one directory per version).
	// Empty means nothing is served and the updater simply finds no manifest.
	DownloadDir string
	// SubstrateCallback is the one non-loopback redirect the device-link flow
	// accepts: "Sign in with Gliph" on Substrate (devicelink.go, substrate.go).
	SubstrateCallback string
	Files             files // upload and voice-note bytes: disk or S3 (files.go)

	Name      string
	Version   string // stamped by the build; "dev" otherwise
	Started   time.Time
	UploadMax int64
	UploadTTL time.Duration
	Limits    *Limits
}

func NewServer(st *Store, name string) *Server {
	s := &Server{St: st, Reg: NewRegistry(), Blobs: NewBlobRegistry(),
		conns: map[string][]*Conn{}, plugins: map[string]*PluginConn{},
		offered:    map[offeredKey]map[string]string{},
		blobCalls:  map[string]chan blobReply{},
		roomJoined: map[*Conn]map[string]*roomSeat{},
		roomSince:  map[string]string{},
		Name:       name, UploadMax: 1 << 30, UploadTTL: 7 * 24 * time.Hour, Limits: NewLimits(),
		Mail: noMail{}}
	registerBuiltins(s.Reg)
	registerBlobCommands(s.Reg)
	registerRoomCommands(s.Reg)
	registerBuiltinBlobs(s)
	// A plugin registering or dropping blobs changes what the switcher shows,
	// so the affected clients need fresh channel state.
	s.Blobs.OnChange(func() {
		for _, uid := range s.onlineUsers() {
			s.PushState(uid)
		}
	})
	// Whenever the registry changes (plugin connects or drops) every client
	// gets a fresh palette. This is what makes commands extensible at runtime.
	s.Reg.OnChange(s.BroadcastCommands)
	return s
}

// ---- connection registry ----

// addConn registers a connection, refusing it past the per-user cap.
func (s *Server) addConn(c *Conn) bool {
	s.mu.Lock()
	if len(s.conns[c.user.ID]) >= s.Limits.MaxConns {
		s.mu.Unlock()
		return false
	}
	first := len(s.conns[c.user.ID]) == 0
	s.conns[c.user.ID] = append(s.conns[c.user.ID], c)
	s.mu.Unlock()
	// Notify lists fire on arrival, not per device: opening a second tab is not
	// "coming online".
	if first {
		s.NotifyOnline(c.user.ID)
		s.pushPresence(c.user.ID)
		// Coming back is what ends a digest's silence — see email.go. Stamped
		// on arrival rather than on every frame: while the socket is up the
		// sweep skips the user on IsOnline anyway.
		s.St.MarkSeen(c.user.ID)
	}
	return true
}

func (s *Server) removeConn(c *Conn) {
	s.mu.Lock()
	list := s.conns[c.user.ID]
	for i, x := range list {
		if x == c {
			s.conns[c.user.ID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	last := len(s.conns[c.user.ID]) == 0
	if last {
		delete(s.conns, c.user.ID)
		// `offered` grew an entry for every (channel, blob, user) that ever
		// rendered. Reopening a blob publishes a fresh document, so dropping
		// them on the last disconnect loses nothing. Channel invites are NOT
		// cleaned up here: they live in the DB and are spent by the join or
		// the kick, so an invitee may close the tab and come back tomorrow.
		for k := range s.offered {
			if k.uid == c.user.ID {
				delete(s.offered, k)
			}
		}
	}
	s.mu.Unlock()
	// A closed socket is a walk out of any 3D room it was standing in.
	s.roomDisconnect(c)
	if last {
		s.pushPresence(c.user.ID)
		// The last socket closing is the moment the absence starts, and the
		// digest's quiet period counts from here.
		s.St.MarkSeen(c.user.ID)
	}
	c.close()
}

// requireMember is the ONE place that decides whether someone may act in a
// channel. Every entry point taking a client-supplied channel id goes through
// it — message.send, command.run, the blob router, the key frames, the p2p
// relay — because this question answered independently in five places is a
// question four of them get right.
//
// The fifth was command.run: a non-member scans as role "", which ranks equal
// to "member" (see Allowed), so every member-level command ran for anyone
// holding a channel id — including someone who had just been kicked out of it.
func (s *Server) requireMember(cid, uid string) (string, error) {
	role, ok := s.St.MemberRole(cid, uid)
	if !ok {
		return "", errors.New("you are not in that channel")
	}
	return role, nil
}

// onlineUsers is every user with at least one live connection.
func (s *Server) onlineUsers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.conns))
	for uid := range s.conns {
		out = append(out, uid)
	}
	return out
}

func (s *Server) IsOnline(uid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.conns[uid]) > 0
}

func (s *Server) SendToUser(uid string, f Frame) {
	s.mu.RLock()
	list := append([]*Conn(nil), s.conns[uid]...)
	s.mu.RUnlock()
	for _, c := range list {
		c.Send(f)
	}
}

// Broadcast sends one frame to every connected client.
func (s *Server) Broadcast(f Frame) {
	s.mu.RLock()
	all := make([]*Conn, 0, len(s.conns))
	for _, list := range s.conns {
		all = append(all, list...)
	}
	s.mu.RUnlock()
	for _, c := range all {
		c.Send(f)
	}
}

// BroadcastCommands pushes the current palette to every connected client.
func (s *Server) BroadcastCommands() {
	s.Broadcast(Frame{T: "commands", D: map[string]any{"commands": s.Reg.Manifest()}})
}

// ---- channel invites ----
//
// A +i grant is durable: it used to live in a map, so every deploy of the
// server dropped every pending invitation on the floor.

func (s *Server) AddInvite(cid, uid, byNick string) {
	s.St.AddChannelInvite(cid, uid, byNick)
}

func (s *Server) HasInvite(cid, uid string) bool { return s.St.HasChannelInvite(cid, uid) }

// consumeInvite spends a grant. An invite is for one join: leaving it behind
// meant /kick from a +i channel was undone by walking back in.
func (s *Server) consumeInvite(cid, uid string) { s.St.DeleteChannelInvite(cid, uid) }

// ---- posting events ----

// Post appends an event and fans it out to every member of the channel and to
// subscribed agents.
func (s *Server) Post(ch *Channel, e *Event, nonce string) error {
	e.Channel = ch.ID
	saved, err := s.St.Append(e, nonce)
	if err != nil {
		return err
	}
	mEvents.Add(1)
	// Read the member list ONCE. This is the hottest path on the server and it
	// used to ask twice — here and again inside wakePhones.
	members := s.St.MemberIDs(ch.ID)
	f := Frame{T: "event", D: saved}
	for _, uid := range members {
		s.SendToUser(uid, f)
	}
	s.fanoutToAgents(ch.ID, saved)
	go s.fireWebhooks(ch, saved) // webhooks.go
	// And every member's phone gets a content-free wake, socket or no socket.
	s.wakePhones(members, saved)
	return nil
}

func (s *Server) userChannels(uid string) []*Channel {
	chans, err := s.St.ChannelsFor(uid)
	if err != nil {
		return nil
	}
	return chans
}

// DMChannel finds or creates the 1:1 channel for two users.
func (s *Server) DMChannel(a, b *User) (*Channel, error) {
	lo, hi := a.ID, b.ID
	if lo > hi {
		lo, hi = hi, lo
	}
	name := "dm:" + lo + ":" + hi
	ch, err := s.St.ChannelByName(name)
	if err == nil {
		return ch, nil
	}
	ch, err = s.St.CreateChannel(name, "", true)
	if err != nil {
		return nil, err
	}
	if err := s.St.Join(ch.ID, a.ID, "op"); err != nil {
		return nil, err
	}
	if err := s.St.Join(ch.ID, b.ID, "op"); err != nil {
		return nil, err
	}
	return ch, nil
}

// channelView is what a client needs to render a channel in its rail.
func (s *Server) channelView(ch *Channel, uid string) map[string]any {
	role, _ := s.St.MemberRole(ch.ID, uid)
	maxSeq, _ := s.St.MaxSeq(ch.ID)
	members, _ := s.St.Members(ch.ID)
	for i := range members {
		members[i].Online = s.IsOnline(members[i].UserID)
	}
	name := ch.Name
	if ch.IsDM {
		// a DM is displayed as the other person
		for _, m := range members {
			if m.UserID != uid {
				name = m.Nick
				break
			}
		}
	}
	v := map[string]any{
		"id": ch.ID, "name": name, "topic": ch.Topic, "modes": ch.Modes, "history": ch.History,
		"is_dm": ch.IsDM, "role": role, "seq": maxSeq, "members": members,
		"has_key": ch.Key != "", "blobs": s.blobsFor(ch),
		// How far this user has read, on any of their devices. A client that
		// has been away adopts it and stops showing lines it already read
		// somewhere else; one that has read further pushes its own mark up.
		"read": s.St.ReadSeq(ch.ID, uid),
	}
	// Who is in this channel's 3D room right now. The snapshot only: the
	// comings and goings after it are the room.event relays the client is
	// already receiving. Absent when nobody is inside.
	if occ, since := s.roomOccupants(ch.ID); len(occ) > 0 {
		v["room"] = map[string]any{"since": since, "users": occ}
	}
	return v
}

func (s *Server) stateFrame(u *User) Frame {
	chans := s.userChannels(u.ID)
	views := make([]map[string]any, 0, len(chans))
	for _, ch := range chans {
		views = append(views, s.channelView(ch, u.ID))
	}
	return Frame{T: "state", D: map[string]any{
		"channels": views, "user": u, "prefs": s.UserPrefs(u.ID)}}
}

// PushState re-sends channel list and membership to every device of one user.
func (s *Server) PushState(uid string) {
	// Ask who is here BEFORE building anything. A state frame is a dozen
	// queries per channel, and SendToUser drops it on the floor when the user
	// holds no socket — so PushChannelState was paying full price for every
	// offline member of a channel, and /away paid it once per member per
	// channel. Most of a room is usually offline.
	if !s.IsOnline(uid) {
		return
	}
	u, err := s.St.UserByID(uid)
	if err != nil {
		return
	}
	s.SendToUser(uid, s.stateFrame(u))
}

// PushChannelState refreshes one channel for everyone in it.
func (s *Server) PushChannelState(cid string) {
	for _, uid := range s.St.MemberIDs(cid) {
		s.PushState(uid)
	}
}

// ---- client frame dispatch ----

func (s *Server) handleFrame(c *Conn, raw []byte) {
	var f struct {
		T string          `json:"t"`
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		c.Send(Frame{T: "error", D: map[string]any{"code": "bad_frame", "message": "malformed frame"}})
		return
	}
	switch f.T {
	case "ping":
		c.Send(Frame{T: "pong"})

	case "sync":
		var d struct {
			Cursors map[string]int64 `json:"cursors"`
		}
		json.Unmarshal(f.D, &d)
		s.handleSync(c, d.Cursors)

	case "read":
		var d struct {
			Marks map[string]int64 `json:"marks"`
		}
		json.Unmarshal(f.D, &d)
		s.handleRead(c, d.Marks)

	case "message.send":
		var d struct {
			Channel string `json:"channel"`
			Body    string `json:"body"`
			Nonce   string `json:"nonce"`
			// In a +e channel Body is base64 ciphertext and Epoch names the
			// channel key it was encrypted under. The server stores both and
			// reads neither.
			Enc   bool  `json:"enc"`
			Epoch int64 `json:"epoch"`
			// The event this answers, for the client's jump-to-original. The
			// quote itself is in Body, where in a +e channel it stays
			// unreadable; this is only ever an id.
			ReplyTo string `json:"reply_to"`
			// Said from inside the 3D room: which sub-room, and whether the
			// sender is keeping their lines out of the channel's stream. Both
			// ride in the clear beside the ciphertext, which is no new leak —
			// the relay already carries room names — and both are only ever
			// read by the clients.
			Room  string `json:"room"`
			Quiet bool   `json:"quiet"`
		}
		json.Unmarshal(f.D, &d)
		if s.throttled(c, "message", d.Channel) {
			return
		}
		if err := s.handleSend(c, d.Channel, d.Body, d.Nonce, d.Enc, d.Epoch, d.ReplyTo, d.Room, d.Quiet); err != nil {
			s.errTo(c, d.Channel, err)
		}

	case "command.run":
		var d struct {
			Channel string `json:"channel"`
			Name    string `json:"name"`
			Args    string `json:"args"`
		}
		json.Unmarshal(f.D, &d)
		if s.throttled(c, "command", d.Channel) {
			return
		}
		if err := s.RunCommand(c, d.Channel, d.Name, d.Args); err != nil {
			s.errTo(c, d.Channel, err)
		}

	default:
		// device.hello / keys.* live in crypto.go, blob.* in blobs.go
		if s.handleCryptoFrame(c, f.T, f.D) {
			return
		}
		if s.handleBlobFrame(c, f.T, f.D) {
			return
		}
		// p2p.* (WebRTC signalling for direct DM transfers) lives in p2p.go
		if s.handleP2PFrame(c, f.T, f.D) {
			return
		}
		// room.* (the 3D room relay) lives in room.go
		if s.handleRoomFrame(c, f.T, f.D) {
			return
		}
		c.Send(Frame{T: "error", D: map[string]any{"code": "unknown_type", "message": "unknown frame: " + f.T}})
	}
}

func (s *Server) errTo(c *Conn, cid string, err error) {
	c.Send(Frame{T: "notice", D: map[string]any{"channel": cid, "level": "error", "text": err.Error()}})
}

const syncBatch = 300

func (s *Server) handleSync(c *Conn, cursors map[string]int64) {
	c.Send(s.stateFrame(c.user))
	for _, ch := range s.userChannels(c.user.ID) {
		from := cursors[ch.ID]
		if ch.History == "sealed" {
			if js := s.St.JoinedSeq(ch.ID, c.user.ID); js > from {
				from = js
			}
		}
		events, err := s.St.Since(ch.ID, from, syncBatch)
		if err != nil || len(events) == 0 {
			continue
		}
		max, _ := s.St.MaxSeq(ch.ID)
		c.Send(Frame{T: "events", D: map[string]any{
			"channel": ch.ID, "events": events,
			"more": events[len(events)-1].Seq < max,
		}})
		s.St.SaveCursor(ch.ID, c.user.ID, events[len(events)-1].Seq)
	}
}

// handleRead records where the user has read to and tells their other devices.
//
// The relay is the whole point: reading on the phone has to clear the badge on
// the desktop, and waiting for the next state frame to do it is what made read
// state feel broken. Marks only ever move forward (Store.SaveRead), so a device
// coming back with a stale mark cannot un-read anything.
func (s *Server) handleRead(c *Conn, marks map[string]int64) {
	moved := map[string]int64{}
	for cid, seq := range marks {
		// Membership is checked for the same reason every other channel entry
		// point checks it: a channel id from a client is a claim, not a fact.
		if _, err := s.requireMember(cid, c.user.ID); err != nil {
			continue
		}
		// A mark past the newest event in the channel is not a real read
		// position, and storing one is permanent: unread would floor at zero
		// and the digest would count nothing, for that channel, forever. Clamp
		// rather than reject — a client racing a new message is honest, it just
		// cannot know more than the log does.
		if max, err := s.St.MaxSeq(cid); err == nil && seq > max {
			seq = max
		}
		if seq <= s.St.ReadSeq(cid, c.user.ID) {
			continue
		}
		s.St.SaveRead(cid, c.user.ID, seq)
		moved[cid] = seq
	}
	if len(moved) == 0 {
		return
	}
	// Everyone signed in as this user EXCEPT the device that just told us. It
	// already knows; echoing to it would be a frame per read for nothing.
	f := Frame{T: "read", D: map[string]any{"marks": moved}}
	s.mu.RLock()
	list := append([]*Conn(nil), s.conns[c.user.ID]...)
	s.mu.RUnlock()
	for _, other := range list {
		if other != c {
			other.Send(f)
		}
	}
}

func (s *Server) handleSend(c *Conn, cid, body, nonce string, enc bool, epoch int64, replyTo, room string, quiet bool) error {
	body = strings.TrimRight(body, " \t")
	if body == "" {
		return errors.New("empty message")
	}
	if len(body) > 8000 {
		return errors.New("message too long")
	}
	ch, err := s.St.ChannelByID(cid)
	if err != nil {
		return errors.New("unknown channel")
	}
	role, err := s.requireMember(cid, c.user.ID)
	if err != nil {
		return err
	}
	// +m: only voice and ops may speak
	if ch.HasMode('m') && role != "op" && role != "voice" && c.user.Role != "admin" {
		return fmt.Errorf("#%s is moderated (+m) — you need voice to speak", ch.Name)
	}
	// In a +e channel the body is ciphertext we cannot read. All we can do is
	// insist the client's claim matches the channel's mode and that the payload
	// is well-formed, then store it verbatim.
	if err := validateEncrypted(ch, enc, epoch, body); err != nil {
		return err
	}
	// A room a client invented would be rendered as a mark in every stream, so
	// an unknown one is refused rather than stamped.
	if room != "" && !validRoom(room) {
		return fmt.Errorf("no such room: %s", room)
	}
	e := &Event{Kind: "message", Actor: c.user.Nick, actorID: c.user.ID,
		Body: body, Enc: enc, Epoch: epoch}
	meta := map[string]any{}
	// A reply may only point at something in this channel. Silently dropped
	// rather than refused: the message is still worth posting, and the client
	// keeps the quote either way — it just loses the jump.
	if replyTo != "" && s.St.HasEvent(cid, replyTo) {
		meta["reply_to"] = replyTo
	}
	if room != "" {
		meta["room"] = room
	}
	if quiet {
		meta["quiet"] = true
	}
	if len(meta) > 0 {
		e.Meta = meta
	}
	return s.Post(ch, e, nonce)
}

// RunCommand resolves a command from the registry, checks scope and role, then
// either runs the builtin handler or routes the call to the owning plugin.
func (s *Server) RunCommand(c *Conn, cid, name, args string) error {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		return errors.New("empty command")
	}
	cmd, ok := s.Reg.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown command: /%s — try /help", name)
	}

	var ch *Channel
	role := ""
	if cid != "" {
		var err error
		if ch, err = s.St.ChannelByID(cid); err == nil {
			// Membership, not just a role lookup. Dropping this check is what
			// let a kicked or banned account keep posting into the room it was
			// removed from: it still holds the channel id from its last state
			// frame, and role "" clears every member-level bar.
			if role, err = s.requireMember(cid, c.user.ID); err != nil {
				return err
			}
		}
	}
	switch cmd.Scope {
	case "channel":
		if ch == nil || ch.IsDM {
			return fmt.Errorf("/%s only works in a channel", name)
		}
	case "dm":
		if ch == nil || !ch.IsDM {
			return fmt.Errorf("/%s only works in a direct message", name)
		}
	}
	if !Allowed(cmd, c.user.Role, role) {
		return fmt.Errorf("/%s needs %s in this channel", name, cmd.MinRole)
	}

	ctx := &Ctx{Srv: s, User: c.user, Channel: ch, Role: role,
		Raw: strings.TrimSpace(args), Args: strings.Fields(args), Conn: c}

	// Required-argument check driven by the same manifest the client renders.
	for i, a := range cmd.Args {
		if a.Required && ctx.Arg(i) == "" {
			return fmt.Errorf("usage: %s", cmd.Usage)
		}
	}

	if cmd.Handler != nil {
		return cmd.Handler(ctx)
	}
	return s.invokePlugin(cmd, ctx)
}

// ---- websocket loop ----

func (s *Server) serveConn(ctx context.Context, ws *websocket.Conn, u *User, client string) {
	// Connecting is what keeps a push registration alive: a token belonging to
	// an app nobody has opened in months gets swept (see PrunePushDevices).
	s.St.touchPushDevices(u.ID)
	c := &Conn{srv: s, ws: ws, user: u, client: client, out: make(chan Frame, 64)}
	first := s.St.NeverSeen(u.ID) // before addConn marks it seen
	if !s.addConn(c) {
		ws.Close(websocket.StatusPolicyViolation, "too many connections for this account")
		return
	}
	defer s.removeConn(c)

	go func() {
		for f := range c.out {
			b, err := json.Marshal(f)
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		}
	}()

	c.Send(Frame{T: "hello", D: map[string]any{
		"first":    first,
		"user":     u,
		"commands": s.Reg.Manifest(),
		"server": map[string]any{
			"name": s.Name, "version": s.Version, "upload_max": s.UploadMax,
			// The two system-message sources (system.go). `motd` is nil when
			// the operator has not set one.
			"motd": s.motd(), "news": news(),
		},
	}})
	c.Send(s.stateFrame(u))

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		s.handleFrame(c, data)
	}
}

func (s *Server) sweepUploads() {
	for _, u := range s.St.ExpiredUploads() {
		s.Files.remove(u.Path)
		s.St.DeleteUpload(u.ID)
		log.Printf("upload expired: %s", u.ID)
	}
}
