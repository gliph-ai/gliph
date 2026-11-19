package main

// Blobs: interactive mini-apps attached to a channel, rendered from GUM
// documents a provider pushes. Design: docs/plans/gliph-apps.md.
//
// The trust model is the command registry's, because it already works:
// providers declare, the server enforces, clients only render. A provider can
// never reach a channel it is not a member of, an action marked min_role: op is
// refused server-side whatever the client sends, and an action id the current
// document never offered is refused outright — so a client cannot replay a
// privileged action the provider has stopped rendering.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- GUM ----

// Action is the only way anything happens in a blob.
type Action struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	MinRole string `json:"min_role,omitempty"` // member | op | admin
	Confirm string `json:"confirm,omitempty"`  // client-side prompt; the role check is the real gate
	Prompt  string `json:"prompt,omitempty"`   // collect one line and send it as `value`
}

type Item struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Hint    string   `json:"hint,omitempty"`
	State   string   `json:"state,omitempty"` // ok | warn | error | done | open
	Actions []Action `json:"actions,omitempty"`
}

// Block is one GUM block. Only `T` is always meaningful; the rest are set per
// type. Kept as one struct rather than a union because the whole point is that
// it is data the client renders, not a class hierarchy.
type Block struct {
	T           string     `json:"t"` // text | divider | list | table | input | buttons | bar
	ID          string     `json:"id,omitempty"`
	Body        string     `json:"body,omitempty"`
	Style       string     `json:"style,omitempty"` // plain | info | warn | error
	Label       string     `json:"label,omitempty"`
	Items       []Item     `json:"items,omitempty"`
	Cols        []string   `json:"cols,omitempty"`
	Rows        [][]string `json:"rows,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
	Action      string     `json:"action,omitempty"`
	Actions     []Action   `json:"actions,omitempty"`
}

type Doc struct {
	Title  string  `json:"title"`
	Blocks []Block `json:"blocks"`
}

type Menu struct {
	Item    string   `json:"item"`
	Actions []Action `json:"actions"`
}

// actionIDs is every action the document offers, and the role each needs. The
// router checks an incoming event against this, so a client cannot invoke
// something the current document does not render.
func (d *Doc) actionIDs() map[string]string {
	out := map[string]string{}
	if d == nil {
		return out
	}
	add := func(as []Action) {
		for _, a := range as {
			out[a.ID] = a.MinRole
		}
	}
	for _, b := range d.Blocks {
		add(b.Actions)
		if b.Action != "" {
			if _, ok := out[b.Action]; !ok {
				out[b.Action] = ""
			}
		}
		for _, it := range b.Items {
			add(it.Actions)
		}
	}
	return out
}

// ---- providers ----

type BlobCtx struct {
	Srv     *Server
	Channel *Channel
	User    *User
	Role    string // caller's role in Channel
	Conn    *Conn
	Inst    string // session id for private blobs
}

type BlobEvent struct {
	Action string `json:"action"`
	Item   string `json:"item,omitempty"`
	Value  string `json:"value,omitempty"`
}

// Provider renders a blob and handles its events. Builtins implement this
// directly; a plugin-backed blob is a thin implementation that forwards over
// the plugin socket. Nothing above the router knows which is which.
type Provider interface {
	Open(ctx *BlobCtx) (*Doc, error)
	// Event returns a document (re-render), a menu (transient submenu), or
	// neither (nothing changed on screen — it may have posted to chat instead).
	Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error)
}

// BlobSpec is what clients render in the blob switcher.
type BlobSpec struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Mode   string `json:"mode"`   // shared | private
	Source string `json:"source"` // builtin | <plugin id>

	provider Provider
	// channels limits a plugin blob to the channels it registered for; empty
	// means every channel the provider is a member of.
	channels map[string]bool
}

type BlobRegistry struct {
	mu       sync.RWMutex
	blobs    map[string]*BlobSpec // id -> spec
	onChange func()
}

func NewBlobRegistry() *BlobRegistry { return &BlobRegistry{blobs: map[string]*BlobSpec{}} }

func (r *BlobRegistry) OnChange(f func()) { r.onChange = f }

func (r *BlobRegistry) AddBuiltin(id, title, mode string, p Provider) {
	r.mu.Lock()
	r.blobs[id] = &BlobSpec{ID: id, Title: title, Mode: mode, Source: "builtin", provider: p}
	r.mu.Unlock()
}

func (r *BlobRegistry) RegisterPlugin(pluginID string, specs []*BlobSpec) error {
	r.mu.Lock()
	for _, sp := range specs {
		if ex, clash := r.blobs[sp.ID]; clash && ex.Source != pluginID {
			r.mu.Unlock()
			return fmt.Errorf("blob %q is already registered by %s", sp.ID, ex.Source)
		}
	}
	for _, sp := range specs {
		sp.Source = pluginID
		if sp.Mode == "" {
			sp.Mode = "private"
		}
		if sp.Title == "" {
			sp.Title = sp.ID
		}
		r.blobs[sp.ID] = sp
	}
	r.mu.Unlock()
	if r.onChange != nil {
		r.onChange()
	}
	return nil
}

func (r *BlobRegistry) UnregisterPlugin(pluginID string) {
	r.mu.Lock()
	for id, sp := range r.blobs {
		if sp.Source == pluginID {
			delete(r.blobs, id)
		}
	}
	r.mu.Unlock()
	if r.onChange != nil {
		r.onChange()
	}
}

func (r *BlobRegistry) Lookup(id string) (*BlobSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sp, ok := r.blobs[id]
	return sp, ok
}

// For lists the blobs available in one channel, which is what the client's
// switcher renders. Builtins are everywhere; a plugin blob only where its
// plugin is a member and it asked for.
// Offers reports whether this channel actually has that blob. `For` builds the
// switcher; this answers the same question about one spec, so a command can
// refuse to focus an app the channel does not have.
func (r *BlobRegistry) Offers(s *Server, ch *Channel, want *BlobSpec) bool {
	for _, sp := range r.For(s, ch) {
		if sp.ID == want.ID {
			return true
		}
	}
	return false
}

func (r *BlobRegistry) For(s *Server, ch *Channel) []*BlobSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*BlobSpec{}
	for _, sp := range r.blobs {
		if sp.Source == "builtin" {
			out = append(out, sp)
			continue
		}
		if len(sp.channels) > 0 && !sp.channels[ch.ID] {
			continue
		}
		if s.St.PluginInChannel(sp.Source, ch.ID) {
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- shared documents ----

func (s *Store) PutBlobDoc(cid, blobID string, doc *Doc) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO blob_docs(channel_id,blob_id,doc,updated_at) VALUES(?,?,?,?)
	  ON CONFLICT(channel_id,blob_id) DO UPDATE SET doc=excluded.doc, updated_at=excluded.updated_at`,
		cid, blobID, string(b), now())
	return err
}

func (s *Store) BlobDoc(cid, blobID string) (*Doc, error) {
	var raw string
	if err := s.db.QueryRow(`SELECT doc FROM blob_docs WHERE channel_id=? AND blob_id=?`,
		cid, blobID).Scan(&raw); err != nil {
		return nil, ErrNotFound
	}
	var d Doc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Publish replaces a shared blob's document and pushes it to every member.
func (s *Server) PublishBlob(cid, blobID string, doc *Doc) error {
	if err := s.St.PutBlobDoc(cid, blobID, doc); err != nil {
		return err
	}
	s.rememberOffered(cid, blobID, doc)
	f := Frame{T: "blob.render", D: map[string]any{"channel": cid, "blob": blobID, "doc": doc}}
	for _, uid := range s.St.MemberIDs(cid) {
		s.SendToUser(uid, f)
	}
	return nil
}

// ---- routing ----

// offered remembers the actions the last document for a (channel, blob, user)
// actually rendered, so an event naming anything else is refused.
type offeredKey struct{ cid, blob, uid string }

func (s *Server) rememberOffered(cid, blobID string, doc *Doc) {
	ids := doc.actionIDs()
	// The member list is read BEFORE the lock is taken. s.mu guards the whole
	// hub — every Send, every connect, every plugin dispatch waits on it — so a
	// database round-trip inside it serialised the server on one SQLite read,
	// once per shared-blob publish.
	members := s.St.MemberIDs(cid)
	s.mu.Lock()
	for _, uid := range members {
		s.offered[offeredKey{cid, blobID, uid}] = ids
	}
	s.mu.Unlock()
}

func (s *Server) rememberOfferedFor(cid, blobID, uid string, doc *Doc) {
	ids := doc.actionIDs()
	s.mu.Lock()
	s.offered[offeredKey{cid, blobID, uid}] = ids
	s.mu.Unlock()
}

func (s *Server) offeredAction(cid, blobID, uid, action string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids, ok := s.offered[offeredKey{cid, blobID, uid}]
	if !ok {
		return "", false
	}
	role, ok := ids[action]
	return role, ok
}

// blobContext resolves and authorises a blob request.
func (s *Server) blobContext(c *Conn, cid, blobID string) (*BlobSpec, *BlobCtx, error) {
	sp, ok := s.Blobs.Lookup(blobID)
	if !ok {
		return nil, nil, fmt.Errorf("no such blob: %s", blobID)
	}
	ch, err := s.St.ChannelByID(cid)
	if err != nil {
		return nil, nil, errors.New("unknown channel")
	}
	role, err := s.requireMember(cid, c.user.ID)
	if err != nil {
		return nil, nil, err
	}
	if sp.Source != "builtin" {
		if len(sp.channels) > 0 && !sp.channels[ch.ID] {
			return nil, nil, fmt.Errorf("%s is not available here", blobID)
		}
		if !s.St.PluginInChannel(sp.Source, ch.ID) {
			return nil, nil, fmt.Errorf("%s is not a member of this channel", sp.Source)
		}
	}
	return sp, &BlobCtx{Srv: s, Channel: ch, User: c.user, Role: role, Conn: c,
		Inst: cid + ":" + blobID + ":" + c.user.ID}, nil
}

func (s *Server) handleBlobFrame(c *Conn, typ string, raw json.RawMessage) bool {
	switch typ {
	case "blob.open":
		var d struct {
			Channel string `json:"channel"`
			Blob    string `json:"blob"`
		}
		json.Unmarshal(raw, &d)
		sp, ctx, err := s.blobContext(c, d.Channel, d.Blob)
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		// A shared blob renders the stored document if there is one, so a
		// second viewer sees exactly what the first sees.
		if sp.Mode == "shared" {
			if doc, err := s.St.BlobDoc(d.Channel, d.Blob); err == nil {
				s.rememberOfferedFor(d.Channel, d.Blob, c.user.ID, doc)
				c.Send(Frame{T: "blob.render", D: map[string]any{
					"channel": d.Channel, "blob": d.Blob, "doc": doc}})
				return true
			}
		}
		doc, err := sp.provider.Open(ctx)
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		if doc == nil {
			return true
		}
		if sp.Mode == "shared" {
			s.PublishBlob(d.Channel, d.Blob, doc)
			return true
		}
		s.rememberOfferedFor(d.Channel, d.Blob, c.user.ID, doc)
		c.Send(Frame{T: "blob.render", D: map[string]any{
			"channel": d.Channel, "blob": d.Blob, "doc": doc}})
		return true

	case "blob.event":
		var d struct {
			Channel string `json:"channel"`
			Blob    string `json:"blob"`
			Action  string `json:"action"`
			Item    string `json:"item"`
			Value   string `json:"value"`
		}
		json.Unmarshal(raw, &d)
		sp, ctx, err := s.blobContext(c, d.Channel, d.Blob)
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		// The action must be one the document we last sent this user actually
		// offered, and they must clear its role bar.
		minRole, offered := s.offeredAction(d.Channel, d.Blob, c.user.ID, d.Action)
		if !offered {
			s.errTo(c, d.Channel, fmt.Errorf("action %q is not available", d.Action))
			return true
		}
		if minRole != "" && !Allowed(&Command{MinRole: minRole}, c.user.Role, ctx.Role) {
			s.errTo(c, d.Channel, fmt.Errorf("%s needs %s in this channel", d.Action, minRole))
			return true
		}
		if len(d.Value) > 4000 {
			s.errTo(c, d.Channel, errors.New("value too long"))
			return true
		}
		doc, menu, err := sp.provider.Event(ctx, BlobEvent{Action: d.Action, Item: d.Item, Value: d.Value})
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		switch {
		case menu != nil:
			c.Send(Frame{T: "blob.menu", D: map[string]any{
				"channel": d.Channel, "blob": d.Blob, "item": menu.Item, "actions": menu.Actions}})
			// a menu's actions must also be invokable
			s.mu.Lock()
			ids := s.offered[offeredKey{d.Channel, d.Blob, c.user.ID}]
			if ids == nil {
				ids = map[string]string{}
			}
			for _, a := range menu.Actions {
				ids[a.ID] = a.MinRole
			}
			s.offered[offeredKey{d.Channel, d.Blob, c.user.ID}] = ids
			s.mu.Unlock()
		case doc != nil && sp.Mode == "shared":
			s.PublishBlob(d.Channel, d.Blob, doc)
		case doc != nil:
			s.rememberOfferedFor(d.Channel, d.Blob, c.user.ID, doc)
			c.Send(Frame{T: "blob.render", D: map[string]any{
				"channel": d.Channel, "blob": d.Blob, "doc": doc}})
		}
		return true
	}
	return false
}

// blobsFor is the switcher payload for one channel.
func (s *Server) blobsFor(ch *Channel) []map[string]any {
	out := []map[string]any{}
	for _, sp := range s.Blobs.For(s, ch) {
		out = append(out, map[string]any{
			"id": sp.ID, "title": sp.Title, "mode": sp.Mode, "source": sp.Source})
	}
	return out
}

// ---- plugin-backed blobs ----

// pluginProvider forwards a blob's open/event calls over the plugin socket and
// waits for the plugin to answer. It satisfies the same Provider interface a
// builtin does, which is the whole point: the router cannot tell them apart.
type pluginProvider struct {
	srv      *Server
	pluginID string
	blobID   string
}

// call sends one frame to the plugin and waits for `render` or `menu`.
func (p pluginProvider) call(ctx *BlobCtx, frame string, ev *BlobEvent) (*Doc, *Menu, error) {
	s := p.srv
	s.mu.RLock()
	conn := s.plugins[p.pluginID]
	s.mu.RUnlock()
	if conn == nil {
		return nil, nil, fmt.Errorf("%s is not connected", p.pluginID)
	}

	call := id("b")
	ch := make(chan blobReply, 1)
	s.mu.Lock()
	s.blobCalls[call] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.blobCalls, call)
		s.mu.Unlock()
	}()

	d := map[string]any{
		"call": call, "inst": ctx.Inst, "channel": ctx.Channel.ID, "blob": p.blobID,
		"actor": ctx.User.Nick, "role": ctx.Role,
	}
	if ev != nil {
		d["action"], d["item"], d["value"] = ev.Action, ev.Item, ev.Value
	}
	conn.Send(Frame{T: frame, D: d})

	// ponytail: same fixed 30s ceiling as a command invoke. A provider that
	// needs longer should render a "working…" document and publish when done.
	select {
	case r := <-ch:
		if r.err != "" {
			return nil, nil, errors.New(r.err)
		}
		return r.doc, r.menu, nil
	case <-time.After(30 * time.Second):
		return nil, nil, fmt.Errorf("%s did not answer in time", p.pluginID)
	}
}

func (p pluginProvider) Open(ctx *BlobCtx) (*Doc, error) {
	doc, _, err := p.call(ctx, "blob_open", nil)
	return doc, err
}

func (p pluginProvider) Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error) {
	return p.call(ctx, "blob_event", &ev)
}

type blobReply struct {
	doc  *Doc
	menu *Menu
	err  string
}

// RegisterPluginBlobs is called from the plugin `register` frame.
func (s *Server) RegisterPluginBlobs(pluginID string, raw []json.RawMessage) error {
	specs := make([]*BlobSpec, 0, len(raw))
	for _, r := range raw {
		var d struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Mode     string   `json:"mode"`
			Channels []string `json:"channels"`
		}
		if err := json.Unmarshal(r, &d); err != nil || d.ID == "" {
			return errors.New("a blob needs an id")
		}
		sp := &BlobSpec{ID: d.ID, Title: d.Title, Mode: d.Mode,
			provider: pluginProvider{srv: s, pluginID: pluginID, blobID: d.ID}}
		if len(d.Channels) > 0 {
			sp.channels = map[string]bool{}
			for _, want := range d.Channels {
				ch, err := s.St.ChannelByID(want)
				if err != nil {
					if ch, err = s.St.ChannelByName(want); err != nil {
						continue
					}
				}
				sp.channels[ch.ID] = true
			}
		}
		specs = append(specs, sp)
	}
	return s.Blobs.RegisterPlugin(pluginID, specs)
}

// handlePluginBlobReply routes a plugin's answer back to the waiting call.
// Returns false if the frame is not one of ours.
func (s *Server) handlePluginBlobReply(p *PluginConn, typ string, raw json.RawMessage) bool {
	switch typ {
	case "render", "menu", "blob_fail":
		var d struct {
			Call    string   `json:"call"`
			Doc     *Doc     `json:"doc"`
			Item    string   `json:"item"`
			Actions []Action `json:"actions"`
			Message string   `json:"message"`
		}
		json.Unmarshal(raw, &d)
		s.mu.RLock()
		ch := s.blobCalls[d.Call]
		s.mu.RUnlock()
		if ch == nil {
			return true // expired or unknown call: nothing to answer
		}
		r := blobReply{}
		switch typ {
		case "render":
			r.doc = d.Doc
		case "menu":
			r.menu = &Menu{Item: d.Item, Actions: d.Actions}
		default:
			r.err = d.Message
			if r.err == "" {
				r.err = "the provider refused"
			}
		}
		select {
		case ch <- r:
		default:
		}
		return true

	case "publish":
		// Unsolicited: a shared blob's document changed on the provider's side.
		var d struct {
			Channel string `json:"channel"`
			Blob    string `json:"blob"`
			Doc     *Doc   `json:"doc"`
		}
		json.Unmarshal(raw, &d)
		if d.Doc == nil {
			return true
		}
		cid := d.Channel
		if _, err := s.St.ChannelByID(cid); err != nil {
			ch, err := s.St.ChannelByName(d.Channel)
			if err != nil {
				return true
			}
			cid = ch.ID
		}
		// A provider may only publish into a channel it is a member of.
		if !s.St.PluginInChannel(p.meta.ID, cid) {
			return true
		}
		if sp, ok := s.Blobs.Lookup(d.Blob); !ok || sp.Source != p.meta.ID {
			return true
		}
		s.PublishBlob(cid, d.Blob, d.Doc)
		return true
	}
	return false
}

// ---- helpers for builtin providers ----

func text(body, style string) Block      { return Block{T: "text", Body: body, Style: style} }
func divider(label string) Block         { return Block{T: "divider", Label: label} }
func bar(body string) Block              { return Block{T: "bar", Body: body} }
func buttons(as ...Action) Block         { return Block{T: "buttons", Actions: as} }
func list(id string, items []Item) Block { return Block{T: "list", ID: id, Items: items} }

func input(id, label, placeholder, action string) Block {
	return Block{T: "input", ID: id, Label: label, Placeholder: placeholder, Action: action}
}

// clean trims and caps free text coming from a client before it is stored or
// rendered back to everyone.
func clean(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 32 {
			return r
		}
		return -1
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
