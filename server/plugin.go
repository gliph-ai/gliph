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

// PluginConn is a connected plugin or agent. It owns the commands it
// registered and, if it is an agent, receives events from its channels.
type PluginConn struct {
	srv  *Server
	ws   *websocket.Conn
	meta *Plugin
	out  chan Frame
	once sync.Once
	// sendMu guards out against close: a send after close would panic.
	sendMu  sync.Mutex
	closed  bool
	nCmds   int
	pending sync.Map // call id -> *pendingCall
}

type pendingCall struct {
	conn    *Conn
	channel string
	user    *User
	created time.Time
}

func (p *PluginConn) Send(f Frame) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.out <- f:
	default:
	}
}

// close stops the writer and closes the socket. Closing the socket matters
// when a newer connection replaces this one: otherwise the old client keeps
// a connection the server no longer reads, believes it is registered, and
// never reconnects, while its commands vanish when the newer one drops.
func (p *PluginConn) close() {
	p.once.Do(func() {
		p.sendMu.Lock()
		p.closed = true
		close(p.out)
		p.sendMu.Unlock()
		if p.ws != nil {
			p.ws.CloseNow()
		}
	})
}

type PluginInfo struct {
	ID        string
	Display   string
	Kind      string
	NCommands int
}

func (s *Server) ConnectedPlugins() []PluginInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PluginInfo, 0, len(s.plugins))
	for _, p := range s.plugins {
		out = append(out, PluginInfo{ID: p.meta.ID, Display: p.meta.Display,
			Kind: p.meta.Kind, NCommands: p.nCmds})
	}
	return out
}

// agentBrands are the coding agents and assistants a plugin may name on a
// line it posts (`"agent"` in say/result), so clients draw that product's own
// logo beside it. Anything else is dropped: a plugin cannot make a client
// load an arbitrary image.
var agentBrands = map[string]bool{"claude": true, "openai": true, "gemini": true,
	"cursor": true, "copilot": true, "github": true, "gitlab": true}

func pluginMeta(pid, agent string) map[string]any {
	m := map[string]any{"plugin": pid}
	if agentBrands[agent] {
		m["agent"] = agent
	}
	return m
}

// invokePlugin routes a plugin-owned command call to its connection and waits
// for the plugin to answer with `result` or `fail`.
func (s *Server) invokePlugin(cmd *Command, ctx *Ctx) error {
	s.mu.RLock()
	p := s.plugins[cmd.Source]
	s.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("/%s is unavailable: %s is not connected", cmd.Name, cmd.Source)
	}
	cid := ""
	if ctx.Channel != nil {
		cid = ctx.Channel.ID
		if !s.St.PluginInChannel(p.meta.ID, cid) {
			return fmt.Errorf("%s is not a member of this channel", p.meta.Display)
		}
	}
	call := id("k")
	p.pending.Store(call, &pendingCall{conn: ctx.Conn, channel: cid, user: ctx.User, created: time.Now()})

	// Named arguments, split per the manifest the plugin itself declared.
	argMap := map[string]string{}
	for i, a := range cmd.Args {
		if a.Type == "rest" {
			argMap[a.Name] = ctx.Rest(i)
			break
		}
		argMap[a.Name] = ctx.Arg(i)
	}

	p.Send(Frame{T: "invoke", D: map[string]any{
		"call": call, "channel": cid, "actor": ctx.User.Nick,
		"name": cmd.Name, "args": ctx.Raw, "arg_map": argMap,
	}})

	// ponytail: fixed 30s timeout, no per-command config until someone needs it.
	go func() {
		time.Sleep(30 * time.Second)
		if v, ok := p.pending.LoadAndDelete(call); ok {
			pc := v.(*pendingCall)
			pc.conn.Send(Frame{T: "notice", D: map[string]any{
				"channel": pc.channel, "level": "error",
				"text": fmt.Sprintf("/%s timed out waiting for %s", cmd.Name, p.meta.Display)}})
		}
	}()
	return nil
}

// fanoutToAgents streams a channel event to every agent subscribed to it.
func (s *Server) fanoutToAgents(cid string, e *Event) {
	s.mu.RLock()
	list := make([]*PluginConn, 0, len(s.plugins))
	for _, p := range s.plugins {
		if p.meta.Kind == "agent" {
			list = append(list, p)
		}
	}
	s.mu.RUnlock()
	for _, p := range list {
		if s.St.PluginInChannel(p.meta.ID, cid) {
			p.Send(Frame{T: "event", D: e})
		}
	}
}

// dropPlugin cuts a plugin's live connection. The admin console calls it when
// a plugin is revoked so a running bot cannot keep serving on a token that no
// longer exists; servePlugin's defer does the unregistering from there.
func (s *Server) dropPlugin(id string) {
	s.mu.RLock()
	p := s.plugins[id]
	s.mu.RUnlock()
	if p == nil {
		return
	}
	if p.ws != nil {
		p.ws.CloseNow()
	}
	p.close()
}

func (s *Server) servePlugin(ctx context.Context, ws *websocket.Conn, meta *Plugin) {
	p := &PluginConn{srv: s, ws: ws, meta: meta, out: make(chan Frame, 64)}

	// The handoff: the new connection starts with nothing registered. The old
	// one's commands and blobs go now, and its socket is closed (outside the
	// locks) so its client sees the end and stops believing it is registered.
	s.plugRegMu.Lock()
	s.mu.Lock()
	old := s.plugins[meta.ID]
	s.plugins[meta.ID] = p
	s.mu.Unlock()
	if old != nil {
		s.Reg.UnregisterPlugin(meta.ID)
		s.Blobs.UnregisterPlugin(meta.ID)
	}
	s.plugRegMu.Unlock()
	if old != nil {
		old.close()
	}

	defer func() {
		// Under plugRegMu, so no register from a newer connection can land
		// between the ownership check and the unregister.
		s.plugRegMu.Lock()
		s.mu.Lock()
		owner := s.plugins[meta.ID] == p
		if owner {
			delete(s.plugins, meta.ID)
		}
		s.mu.Unlock()
		if owner {
			// dropping the plugin removes its commands AND its blobs, and
			// repushes both the palette and the affected channels' state
			s.Reg.UnregisterPlugin(meta.ID)
			s.Blobs.UnregisterPlugin(meta.ID)
		}
		s.plugRegMu.Unlock()
		p.close()
		if owner {
			log.Printf("plugin disconnected: %s", meta.ID)
		} else {
			log.Printf("plugin connection replaced: %s", meta.ID)
		}
	}()

	go func() {
		for f := range p.out {
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

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		// Every frame acts only while this connection owns the plugin id, and
		// under plugRegMu so a handoff cannot happen mid-frame: a frame read
		// just before a newer connection took over is dropped.
		s.plugRegMu.Lock()
		s.mu.RLock()
		owner := s.plugins[meta.ID] == p
		s.mu.RUnlock()
		var ferr error
		if owner {
			ferr = s.handlePluginFrame(p, data)
		}
		s.plugRegMu.Unlock()
		if !owner {
			return
		}
		if ferr != nil {
			p.Send(Frame{T: "error", D: map[string]any{"message": ferr.Error()}})
		}
	}
}

func (s *Server) handlePluginFrame(p *PluginConn, raw []byte) error {
	var f struct {
		T string          `json:"t"`
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return errors.New("malformed frame")
	}

	if s.handlePluginBlobReply(p, f.T, f.D) {
		return nil
	}

	switch f.T {
	case "register":
		var d struct {
			Display   string            `json:"display"`
			Kind      string            `json:"kind"`
			Commands  []*Command        `json:"commands"`
			Blobs     []json.RawMessage `json:"blobs"`
			Subscribe []string          `json:"subscribe"`
		}
		if err := json.Unmarshal(f.D, &d); err != nil {
			return errors.New("malformed register")
		}
		for _, c := range d.Commands {
			if c.Name == "" {
				return errors.New("command needs a name")
			}
			c.Handler = nil // plugins never carry server-side handlers
		}
		// The display name is declared here rather than fixed at mint time, so a
		// plugin can be renamed without reissuing its token — but it labels every
		// message the plugin posts, so it must not be able to wear a real
		// account's nick. Agent lines carry a badge in the client; a bot calling
		// itself "waseem" is still a lie.
		//
		// `kind` is deliberately NOT taken from the frame. It decides whether the
		// plugin is streamed the events of every channel it subscribes to, which
		// is the admin's call when minting the token, not the plugin's.
		display := strings.TrimSpace(d.Display)
		if display != "" {
			if _, err := s.St.UserByNick(display); err == nil {
				return fmt.Errorf("display %q is taken by a member", display)
			}
		}
		if err := s.Reg.RegisterPlugin(p.meta.ID, d.Commands); err != nil {
			return err
		}
		// meta.Display and nCmds are read by ConnectedPlugins under this lock.
		renamed := display != "" && display != p.meta.Display
		if display != "" {
			if err := s.St.SetPluginDisplay(p.meta.ID, display); err != nil {
				return err
			}
		}
		s.mu.Lock()
		if display != "" {
			p.meta.Display = display
		}
		p.nCmds = len(d.Commands)
		s.mu.Unlock()
		// Blobs register the same way commands do, and disappear the same way.
		if len(d.Blobs) > 0 {
			if err := s.RegisterPluginBlobs(p.meta.ID, d.Blobs); err != nil {
				return err
			}
		}

		// join requested channels, by name or id
		for _, want := range d.Subscribe {
			ch, err := s.St.ChannelByID(want)
			if err != nil {
				if ch, err = s.St.ChannelByName(want); err != nil {
					continue
				}
			}
			// Under pluginMu, and one statement: an op's /kick wins.
			s.pluginMu.Lock()
			joined := s.St.PluginSubscribe(p.meta.ID, ch.ID)
			s.pluginMu.Unlock()
			if joined {
				s.PushChannelState(ch.ID)
			}
		}
		// Member lists already on screen show the new name.
		if renamed {
			for _, cid := range s.St.PluginChannels(p.meta.ID) {
				s.PushChannelState(cid)
			}
		}
		p.Send(Frame{T: "ready", D: map[string]any{"id": p.meta.ID, "commands": p.nCmds}})
		log.Printf("plugin registered: %s (%d commands)", p.meta.ID, p.nCmds)
		return nil

	case "result":
		var d struct {
			Call  string `json:"call"`
			Say   string `json:"say"`
			Agent string `json:"agent"`
		}
		json.Unmarshal(f.D, &d)
		v, ok := p.pending.LoadAndDelete(d.Call)
		if !ok {
			return errors.New("unknown or expired call")
		}
		pc := v.(*pendingCall)
		if d.Say == "" {
			return nil
		}
		if pc.channel == "" {
			pc.conn.Send(Frame{T: "notice", D: map[string]any{
				"channel": "", "level": "info", "text": d.Say}})
			return nil
		}
		ch, err := s.St.ChannelByID(pc.channel)
		if err != nil {
			return err
		}
		// The membership check and the post happen under pluginMu, so a
		// /kick lands entirely before or entirely after: removed since the
		// call was made, the answer goes to the caller alone.
		s.pluginMu.Lock()
		defer s.pluginMu.Unlock()
		if !s.St.PluginInChannel(p.meta.ID, pc.channel) {
			pc.conn.Send(Frame{T: "notice", D: map[string]any{
				"channel": pc.channel, "level": "info", "text": p.meta.Display + ": " + d.Say}})
			return nil
		}
		return s.Post(ch, &Event{Kind: "agent", Actor: p.meta.Display, Body: d.Say,
			Meta: pluginMeta(p.meta.ID, d.Agent)}, "")

	case "fail":
		var d struct {
			Call    string `json:"call"`
			Message string `json:"message"`
		}
		json.Unmarshal(f.D, &d)
		if v, ok := p.pending.LoadAndDelete(d.Call); ok {
			pc := v.(*pendingCall)
			pc.conn.Send(Frame{T: "notice", D: map[string]any{
				"channel": pc.channel, "level": "error",
				"text": p.meta.Display + ": " + d.Message}})
		}
		return nil

	case "say":
		var d struct {
			Channel string `json:"channel"`
			Body    string `json:"body"`
			Agent   string `json:"agent"`
		}
		json.Unmarshal(f.D, &d)
		ch, err := s.St.ChannelByID(d.Channel)
		if err != nil {
			if ch, err = s.St.ChannelByName(d.Channel); err != nil {
				return errors.New("unknown channel")
			}
		}
		if d.Body == "" {
			return errors.New("empty body")
		}
		s.pluginMu.Lock()
		defer s.pluginMu.Unlock()
		if !s.St.PluginInChannel(p.meta.ID, ch.ID) {
			return errors.New("not a member of that channel")
		}
		return s.Post(ch, &Event{Kind: "agent", Actor: p.meta.Display, Body: d.Body,
			Meta: pluginMeta(p.meta.ID, d.Agent)}, "")
	}
	return fmt.Errorf("unknown frame: %s", f.T)
}
