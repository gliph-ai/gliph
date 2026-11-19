package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Arg describes one command argument. Clients use this for autocomplete only;
// the server re-validates every invocation.
type Arg struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // text | nick | channel | int | file | rest
	Required bool   `json:"required"`
}

// Command is one entry in the palette. Handler is nil for plugin-owned
// commands, which are routed to the owning plugin connection instead.
type Command struct {
	Name    string `json:"name"`
	Usage   string `json:"usage"`
	Desc    string `json:"desc"`
	Args    []Arg  `json:"args"`
	Scope   string `json:"scope"`    // channel | dm | any | global
	MinRole string `json:"min_role"` // member | op | admin
	Source  string `json:"source"`   // builtin | <plugin id>

	Handler func(*Ctx) error `json:"-"`
}

// Ctx is everything a command handler needs.
type Ctx struct {
	Srv     *Server
	User    *User
	Channel *Channel
	Role    string // caller's role in Channel
	Raw     string // the whole argument string
	Args    []string
	Conn    *Conn
}

func (c *Ctx) Arg(i int) string {
	if i < len(c.Args) {
		return c.Args[i]
	}
	return ""
}

// Rest returns args from i onward as one string (for message-like arguments).
func (c *Ctx) Rest(i int) string {
	if i < len(c.Args) {
		return strings.Join(c.Args[i:], " ")
	}
	return ""
}

// FocusBlob asks the caller's client to switch to a blob. It is the only
// server-to-client frame that moves the UI, and it exists because a command
// like /todos has to be able to open the thing it names — `blob.open` runs the
// other way, so without this the command could only describe the app it wanted.
//
// It is addressed at ONE connection, not the user's devices: you typed it here,
// so it opens here. A phone in your pocket should not change surface because
// you typed a word on a laptop.
func (c *Ctx) FocusBlob(blob string) error {
	if c.Channel == nil {
		return errors.New("that only works in a channel")
	}
	if sp, ok := c.Srv.Blobs.Lookup(blob); !ok || !c.Srv.Blobs.Offers(c.Srv, c.Channel, sp) {
		return fmt.Errorf("no %s app in this channel", blob)
	}
	c.Conn.Send(Frame{T: "blob.focus", D: map[string]any{
		"channel": c.Channel.ID, "blob": blob}})
	return nil
}

func (c *Ctx) Notice(format string, a ...any) {
	cid := ""
	if c.Channel != nil {
		cid = c.Channel.ID
	}
	c.Conn.Send(Frame{T: "notice", D: map[string]any{
		"channel": cid, "level": "info", "text": fmt.Sprintf(format, a...)}})
}

// Registry holds builtin plus plugin commands and notifies on change.
type Registry struct {
	mu       sync.RWMutex
	builtin  map[string]*Command
	plugin   map[string]*Command // name -> command (source = plugin id)
	onChange func()
}

func NewRegistry() *Registry {
	return &Registry{builtin: map[string]*Command{}, plugin: map[string]*Command{}}
}

func (r *Registry) OnChange(f func()) { r.onChange = f }

func (r *Registry) AddBuiltin(c *Command) {
	c.Source = "builtin"
	r.mu.Lock()
	r.builtin[c.Name] = c
	r.mu.Unlock()
}

// RegisterPlugin adds a plugin's commands. A name colliding with a builtin is
// rejected so a plugin can never shadow /kick or /op.
func (r *Registry) RegisterPlugin(pluginID string, cmds []*Command) error {
	r.mu.Lock()
	for _, c := range cmds {
		if _, clash := r.builtin[c.Name]; clash {
			r.mu.Unlock()
			return fmt.Errorf("command /%s is a builtin", c.Name)
		}
		if ex, clash := r.plugin[c.Name]; clash && ex.Source != pluginID {
			r.mu.Unlock()
			return fmt.Errorf("command /%s already registered by %s", c.Name, ex.Source)
		}
	}
	for _, c := range cmds {
		c.Source = pluginID
		if c.Scope == "" {
			c.Scope = "channel"
		}
		if c.MinRole == "" {
			c.MinRole = "member"
		}
		if c.Usage == "" {
			c.Usage = "/" + c.Name
		}
		r.plugin[c.Name] = c
	}
	r.mu.Unlock()
	if r.onChange != nil {
		r.onChange()
	}
	return nil
}

// UnregisterPlugin drops everything owned by a plugin (called on disconnect).
func (r *Registry) UnregisterPlugin(pluginID string) {
	r.mu.Lock()
	for name, c := range r.plugin {
		if c.Source == pluginID {
			delete(r.plugin, name)
		}
	}
	r.mu.Unlock()
	if r.onChange != nil {
		r.onChange()
	}
}

func (r *Registry) Lookup(name string) (*Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.builtin[name]; ok {
		return c, true
	}
	c, ok := r.plugin[name]
	return c, ok
}

// Manifest is what gets pushed to clients. It is the whole palette; clients
// hardcode nothing.
func (r *Registry) Manifest() []*Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Command, 0, len(r.builtin)+len(r.plugin))
	for _, c := range r.builtin {
		out = append(out, c)
	}
	for _, c := range r.plugin {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var roleRank = map[string]int{"member": 0, "voice": 0, "op": 1, "admin": 2}

// Allowed reports whether a caller holding channelRole (and user role) may run c.
func Allowed(c *Command, userRole, channelRole string) bool {
	have := roleRank[channelRole]
	if roleRank[userRole] > have {
		have = roleRank[userRole] // server admins outrank channel roles
	}
	return have >= roleRank[c.MinRole]
}

// ---- builtins ----

func registerBuiltins(r *Registry) {
	add := func(name, usage, desc, scope, minRole string, args []Arg, h func(*Ctx) error) {
		r.AddBuiltin(&Command{Name: name, Usage: usage, Desc: desc, Scope: scope,
			MinRole: minRole, Args: args, Handler: h})
	}
	nick := func(req bool) []Arg { return []Arg{{Name: "nick", Type: "nick", Required: req}} }

	add("me", "/me <action>", "send an action line", "any", "member",
		[]Arg{{Name: "action", Type: "rest", Required: true}}, cmdMe)

	add("slap", "/slap <nick>", "a large trout, for old times", "any", "member", nick(true), cmdSlap)

	add("join", "/join #channel [key]", "join or create a channel", "global", "member",
		[]Arg{{Name: "channel", Type: "channel", Required: true}, {Name: "key", Type: "text"}}, cmdJoin)

	add("create", "/create #channel [open|sealed]", "create a channel and take ops; sealed: joiners see only what comes after", "global", "member",
		[]Arg{{Name: "channel", Type: "channel", Required: true}, {Name: "history", Type: "text"}}, cmdCreate)

	add("part", "/part [#channel]", "leave a channel", "channel", "member",
		[]Arg{{Name: "channel", Type: "channel"}}, cmdPart)

	add("topic", "/topic <text>", "set the channel topic", "channel", "member",
		[]Arg{{Name: "text", Type: "rest", Required: true}}, cmdTopic)

	add("invite", "/invite <nick>", "invite a user, or add a connected plugin, to this channel", "channel", "op", nick(true), cmdInvite)

	add("kick", "/kick <nick> [reason]", "remove a user, with a reason", "channel", "op",
		[]Arg{{Name: "nick", Type: "nick", Required: true}, {Name: "reason", Type: "rest"}}, cmdKick)

	add("ban", "/ban <nick> [reason]", "ban a user from this channel", "channel", "op",
		[]Arg{{Name: "nick", Type: "nick", Required: true}, {Name: "reason", Type: "rest"}}, cmdBan)

	add("uninvite", "/uninvite <nick>", "revoke a pending invitation", "channel", "op", nick(true), cmdKick)

	add("unban", "/unban <nick>", "lift a ban", "channel", "op", nick(true), cmdUnban)

	add("bans", "/bans", "list this channel's bans", "channel", "member", nil, cmdBans)

	add("op", "/op <nick>", "promote a member to channel op", "channel", "op", nick(true), cmdOp)
	add("deop", "/deop <nick>", "demote a channel op", "channel", "op", nick(true), cmdDeop)
	add("voice", "/voice <nick>", "grant voice in a moderated channel", "channel", "op", nick(true), cmdVoice)
	add("devoice", "/devoice <nick>", "remove voice", "channel", "op", nick(true), cmdDevoice)

	add("mode", "/mode <+/-flags> [key]", "set channel modes: e i m n t s k", "channel", "op",
		[]Arg{{Name: "flags", Type: "text", Required: true}, {Name: "key", Type: "text"}}, cmdMode)

	add("nick", "/nick <newnick>", "change your nick", "global", "member", nick(true), cmdNick)

	add("away", "/away [reason]", "set your away message; bare /away or /back clears it", "global", "member",
		[]Arg{{Name: "reason", Type: "rest"}}, cmdAway)
	add("back", "/back", "clear your away message", "global", "member", nil,
		func(c *Ctx) error { c.Raw = ""; return cmdAway(c) })

	add("whois", "/whois <nick>", "profile, idle, channels, keys", "global", "member", nick(true), cmdWhois)

	add("list", "/list", "browse public channels", "global", "member", nil, cmdList)

	add("msg", "/msg <nick> [text]", "open a direct message", "global", "member",
		[]Arg{{Name: "nick", Type: "nick", Required: true}, {Name: "text", Type: "rest"}}, cmdMsg)

	add("help", "/help [command]", "list commands, or explain one", "any", "member",
		[]Arg{{Name: "command", Type: "text"}}, cmdHelp)

	// Client-side commands. The core handles these on the device and never
	// sends them; they are listed here so the palette and /help know them,
	// and the handler only answers a client too old to know them itself.
	add("export", "/export [txt|jsonl]", "save this channel's history on this device as a file", "any", "member",
		[]Arg{{Name: "format", Type: "text"}}, cmdClientSide)
	add("theme", "/theme [add|remove|<name>]", "install a theme file on this device, or switch to one", "any", "member",
		[]Arg{{Name: "action", Type: "rest"}}, cmdClientSide)
	add("script", "/script [add|edit|remove|on|off|export] <name>", "scripts that run on this device on a highlight, a join or a matching line", "any", "member",
		[]Arg{{Name: "action", Type: "rest"}}, cmdClientSide)

	add("update", "/update", "get the newest desktop build: download, install, restart", "any", "member", nil, cmdUpdate)

	add("status", "/status", "which gliph you and the server are running", "any", "member", nil, cmdStatus)

	add("plugins", "/plugins", "show connected plugins and agents", "global", "member", nil, cmdPlugins)

	registerRetroCommands(r)
	registerTrustCommands(add)
}

func cmdMe(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/me needs a channel")
	}
	if c.Raw == "" {
		return errors.New("usage: /me <action>")
	}
	return c.Srv.Post(c.Channel, &Event{Kind: "action", Actor: c.User.Nick, actorID: c.User.ID, Body: c.Raw}, "")
}

func cmdSlap(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/slap needs a channel")
	}
	target := c.Arg(0)
	if target == "" {
		return errors.New("usage: /slap <nick>")
	}
	body := fmt.Sprintf("slaps %s around a bit with a large trout", target)
	return c.Srv.Post(c.Channel, &Event{Kind: "action", Actor: c.User.Nick, actorID: c.User.ID, Body: body}, "")
}

func cmdJoin(c *Ctx) error {
	name := strings.TrimPrefix(c.Arg(0), "#")
	if name == "" {
		return errors.New("usage: /join #channel [key]")
	}
	ch, err := c.Srv.St.ChannelByName(name)
	if errors.Is(err, ErrNotFound) {
		return createChannel(c, name)
	} else if err != nil {
		return err
	}
	if _, already := c.Srv.St.MemberRole(ch.ID, c.User.ID); already {
		return fmt.Errorf("you are already in #%s", name)
	}
	if c.Srv.St.IsBanned(ch.ID, c.User.Nick, c.User.ID) {
		return fmt.Errorf("you are banned from #%s", name)
	}
	if ch.HasMode('i') && !c.Srv.HasInvite(ch.ID, c.User.ID) {
		return fmt.Errorf("#%s is invite-only", name)
	}
	if ch.Key != "" && subtle.ConstantTimeCompare([]byte(c.Arg(1)), []byte(ch.Key)) != 1 {
		return fmt.Errorf("#%s needs a key", name)
	}
	if err := c.Srv.St.Join(ch.ID, c.User.ID, "member"); err != nil {
		return err
	}
	// An invite is spent by the join it authorised. Keeping it meant a later
	// /kick from a +i channel could be undone by simply walking back in.
	c.Srv.consumeInvite(ch.ID, c.User.ID)
	c.Srv.Post(ch, &Event{Kind: "join", Actor: c.User.Nick}, "")
	c.Srv.PushState(c.User.ID)
	return nil
}

// cmdCreate is /join's create half on its own, for people who want the failure
// when the name is taken instead of silently walking into someone else's room.
func cmdCreate(c *Ctx) error {
	name := strings.TrimPrefix(c.Arg(0), "#")
	if name == "" {
		return errors.New("usage: /create #channel")
	}
	_, err := c.Srv.St.ChannelByName(name)
	if err == nil {
		return fmt.Errorf("#%s already exists — /join #%s", name, name)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	history := strings.ToLower(c.Arg(1))
	if history == "" {
		history = "open"
	}
	return createChannelWith(c, name, history)
}

func createChannel(c *Ctx, name string) error { return createChannelWith(c, name, "open") }

func createChannelWith(c *Ctx, name, history string) error {
	ch, err := c.Srv.St.CreateChannelWith(name, "", false, history)
	if err != nil {
		return err
	}
	// creator owns the room
	if err := c.Srv.St.Join(ch.ID, c.User.ID, "op"); err != nil {
		return err
	}
	c.Srv.Post(ch, &Event{Kind: "join", Actor: c.User.Nick, Body: "created the channel"}, "")
	c.Srv.PushState(c.User.ID)
	if history == "sealed" {
		c.Notice("created #%s. you are op. history is sealed: people who join read only what comes after", name)
	} else {
		c.Notice("created #%s. you are op. history is open: people who join read what came before", name)
	}
	return nil
}

func cmdPart(c *Ctx) error {
	ch := c.Channel
	if n := c.Arg(0); n != "" {
		var err error
		if ch, err = c.Srv.St.ChannelByName(n); err != nil {
			return err
		}
		// This branch takes a NAME, so it reaches any channel on the server by
		// guessing one — RunCommand's membership check only covers the channel
		// the frame named. Without this, /part #execs made Leave a no-op and
		// then posted "*** mallory has left" to a room mallory was never in.
		if _, err := c.Srv.requireMember(ch.ID, c.User.ID); err != nil {
			return err
		}
	}
	if ch == nil {
		return errors.New("usage: /part [#channel]")
	}
	if err := c.Srv.St.Leave(ch.ID, c.User.ID); err != nil {
		return err
	}
	c.Srv.Post(ch, &Event{Kind: "part", Actor: c.User.Nick}, "")
	c.Srv.PushState(c.User.ID)
	return nil
}

func cmdTopic(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/topic needs a channel")
	}
	// +t means only ops may set the topic
	if c.Channel.HasMode('t') && !Allowed(&Command{MinRole: "op"}, c.User.Role, c.Role) {
		return errors.New("this channel is +t: only ops set the topic")
	}
	if c.Raw == "" {
		return errors.New("usage: /topic <text>")
	}
	if err := c.Srv.St.SetTopic(c.Channel.ID, c.Raw); err != nil {
		return err
	}
	c.Channel.Topic = c.Raw
	c.Srv.Post(c.Channel, &Event{Kind: "topic", Actor: c.User.Nick, Body: c.Raw}, "")
	c.Srv.PushChannelState(c.Channel.ID)
	return nil
}

func cmdInvite(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/invite needs a channel")
	}
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		// A connected plugin is added by its name or id: an op's say-so is how
		// a plugin gets into a channel it did not subscribe to (Substrate's
		// `/invite substrate`, docs/protocol.md "Plugins and agents").
		p, perr := connectedPlugin(c.Srv, c.Arg(0))
		if perr != nil {
			return perr
		}
		if p != nil {
			c.Srv.pluginMu.Lock()
			err := c.Srv.St.PluginInvite(p.ID, c.Channel.ID)
			c.Srv.pluginMu.Unlock()
			if err != nil {
				return err
			}
			c.Srv.PushChannelState(c.Channel.ID)
			c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick,
				Body: fmt.Sprintf("added %s to the channel", p.Display)}, "")
			c.Notice("added %s; its commands work here now. /kick %s removes it", p.Display, p.ID)
			return nil
		}
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	c.Srv.AddInvite(c.Channel.ID, u.ID, c.User.Nick)
	c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick,
		Body: fmt.Sprintf("invited %s to the channel", u.Nick)}, "")
	c.Srv.SendToUser(u.ID, Frame{T: "notice", D: map[string]any{
		"channel": "", "level": "info",
		"text": fmt.Sprintf("%s invited you to #%s — /join #%s", c.User.Nick, c.Channel.Name, c.Channel.Name)}})
	c.Notice("invited %s", u.Nick)
	return nil
}

// connectedPlugin finds a connected plugin by id, or by its registered name
// when exactly one plugin has it. Display names are chosen at register, so an
// ambiguous one is refused rather than guessed.
func connectedPlugin(s *Server, name string) (*PluginInfo, error) {
	var byName []PluginInfo
	for _, p := range s.ConnectedPlugins() {
		if p.ID == name {
			return &p, nil
		}
		if strings.EqualFold(p.Display, name) {
			byName = append(byName, p)
		}
	}
	switch len(byName) {
	case 0:
		return nil, nil
	case 1:
		return &byName[0], nil
	}
	return nil, fmt.Errorf("more than one plugin is called %s; use its id (/plugins lists them)", name)
}

func cmdKick(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/kick needs a channel")
	}
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		// A plugin in this channel is removed the same way, by name or id.
		pid := ""
		if p, perr := connectedPlugin(c.Srv, c.Arg(0)); perr != nil {
			return perr
		} else if p != nil {
			pid = p.ID
		} else if c.Srv.St.PluginInChannel(c.Arg(0), c.Channel.ID) {
			pid = c.Arg(0)
		}
		if pid == "" || !c.Srv.St.PluginInChannel(pid, c.Channel.ID) {
			return fmt.Errorf("no such user: %s", c.Arg(0))
		}
		c.Srv.pluginMu.Lock()
		err := c.Srv.St.PluginLeave(pid, c.Channel.ID)
		c.Srv.pluginMu.Unlock()
		if err != nil {
			return err
		}
		c.Srv.PushChannelState(c.Channel.ID)
		c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick,
			Body: fmt.Sprintf("removed %s from the channel", c.Arg(0))}, "")
		return nil
	}
	if _, ok := c.Srv.St.MemberRole(c.Channel.ID, u.ID); !ok {
		// Not in yet, but on their way in: the same command revokes the +i
		// grant. Otherwise a misfired /invite could not be taken back at all.
		if !c.Srv.HasInvite(c.Channel.ID, u.ID) {
			return fmt.Errorf("%s is not in this channel", u.Nick)
		}
		c.Srv.consumeInvite(c.Channel.ID, u.ID)
		c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick,
			Body: fmt.Sprintf("revoked %s's invitation", u.Nick)}, "")
		c.Notice("revoked %s's invitation to #%s", u.Nick, c.Channel.Name)
		return nil
	}
	reason := c.Rest(1)
	if reason == "" {
		reason = "no reason given"
	}
	if err := c.Srv.St.Leave(c.Channel.ID, u.ID); err != nil {
		return err
	}
	// Drop any +i grant with them, or the kick lasts until they type /join.
	c.Srv.consumeInvite(c.Channel.ID, u.ID)
	c.Srv.Post(c.Channel, &Event{Kind: "kick", Actor: c.User.Nick, Body: reason,
		Meta: map[string]any{"target": u.Nick}}, "")
	c.Srv.PushState(u.ID)
	return nil
}

func cmdBan(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/ban needs a channel")
	}
	target := c.Arg(0)
	if target == "" {
		return errors.New("usage: /ban <nick> [reason]")
	}
	reason := c.Rest(1)
	// Resolve the account BEFORE writing the ban, so the row carries an id and
	// not just a nick. A nick ban was one /nick away from meaningless. An
	// unregistered nick still bans — it just has no id to follow.
	uid := ""
	u, uerr := c.Srv.St.UserByNick(target)
	if uerr == nil {
		uid = u.ID
	}
	if err := c.Srv.St.Ban(c.Channel.ID, target, uid, reason, c.User.Nick); err != nil {
		return err
	}
	if uerr == nil {
		c.Srv.St.Leave(c.Channel.ID, u.ID)
		// A ban is also an eviction from any +i grant they were holding.
		c.Srv.consumeInvite(c.Channel.ID, u.ID)
		c.Srv.PushState(u.ID)
	}
	c.Srv.Post(c.Channel, &Event{Kind: "mode", Actor: c.User.Nick,
		Body: fmt.Sprintf("sets ban on %s (%s)", target, reason)}, "")
	return nil
}

func cmdUnban(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/unban needs a channel")
	}
	if err := c.Srv.St.Unban(c.Channel.ID, c.Arg(0)); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "mode", Actor: c.User.Nick,
		Body: fmt.Sprintf("lifts ban on %s", c.Arg(0))}, "")
	return nil
}

func cmdBans(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/bans needs a channel")
	}
	bans := c.Srv.St.Bans(c.Channel.ID)
	if len(bans) == 0 {
		c.Notice("no bans in #%s", c.Channel.Name)
		return nil
	}
	var b strings.Builder
	b.WriteString("bans in #" + c.Channel.Name + ":")
	for _, x := range bans {
		b.WriteString(fmt.Sprintf("\n  %s — %s (by %s)", x["nick"], x["reason"], x["by"]))
	}
	c.Notice("%s", b.String())
	return nil
}

func setRole(c *Ctx, role, verb string) error {
	if c.Channel == nil {
		return errors.New("this command needs a channel")
	}
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	if err := c.Srv.St.SetMemberRole(c.Channel.ID, u.ID, role); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "mode", Actor: c.User.Nick,
		Body: fmt.Sprintf("sets mode: %s %s", verb, u.Nick), Meta: map[string]any{"target": u.Nick}}, "")
	c.Srv.PushChannelState(c.Channel.ID)
	return nil
}

func cmdOp(c *Ctx) error      { return setRole(c, "op", "+o") }
func cmdDeop(c *Ctx) error    { return setRole(c, "member", "-o") }
func cmdVoice(c *Ctx) error   { return setRole(c, "voice", "+v") }
func cmdDevoice(c *Ctx) error { return setRole(c, "member", "-v") }

func cmdMode(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/mode needs a channel")
	}
	spec := c.Arg(0)
	if len(spec) < 2 || (spec[0] != '+' && spec[0] != '-') {
		return errors.New("usage: /mode +im  or  /mode -i  (flags: i m n t s k)")
	}
	adding := spec[0] == '+'
	modes := c.Channel.Modes
	for _, f := range spec[1:] {
		// No space in this set. It used to have one, which made `/mode "+ "`
		// store a space as a mode flag.
		if !strings.ContainsRune("eimntsk", f) {
			return fmt.Errorf("unknown mode: %c", f)
		}
		if f == 'k' {
			if adding {
				key := c.Arg(1)
				if key == "" {
					return errors.New("+k needs a key: /mode +k <key>")
				}
				c.Srv.St.SetChannelKey(c.Channel.ID, key)
			} else {
				c.Srv.St.SetChannelKey(c.Channel.ID, "")
			}
		}
		if adding && !strings.ContainsRune(modes, f) {
			modes += string(f)
		} else if !adding {
			modes = strings.ReplaceAll(modes, string(f), "")
		}
	}
	if err := c.Srv.St.SetModes(c.Channel.ID, modes); err != nil {
		return err
	}
	c.Channel.Modes = modes
	c.Srv.Post(c.Channel, &Event{Kind: "mode", Actor: c.User.Nick,
		Body: fmt.Sprintf("sets mode: %s", spec)}, "")
	c.Srv.PushChannelState(c.Channel.ID)
	return nil
}

func cmdNick(c *Ctx) error {
	old := c.User.Nick
	n := c.Arg(0)
	if n == "" {
		return errors.New("usage: /nick <newnick>")
	}
	if err := c.Srv.St.SetNick(c.User.ID, n); err != nil {
		return err
	}
	c.User.Nick = n
	for _, ch := range c.Srv.userChannels(c.User.ID) {
		c.Srv.Post(ch, &Event{Kind: "notice", Actor: old,
			Body: fmt.Sprintf("is now known as %s", n)}, "")
	}
	c.Srv.PushState(c.User.ID)
	return nil
}

func cmdAway(c *Ctx) error {
	if err := c.Srv.St.SetAway(c.User.ID, c.Raw); err != nil {
		return err
	}
	c.User.Away = c.Raw
	if c.Raw == "" {
		c.Notice("you are no longer marked away")
	} else {
		c.Notice("you are away: %s", c.Raw)
	}
	// One refresh per PERSON, not one per shared channel. /away changes a single
	// field on a single user, and PushChannelState-per-channel re-sent the whole
	// state frame to everyone in every room this user is in — so somebody in
	// twenty channels of fifty people cost fifty thousand queries to say "brb".
	// PushState now also returns early for anyone offline, which is most of them.
	seen := map[string]bool{c.User.ID: true}
	for _, ch := range c.Srv.userChannels(c.User.ID) {
		for _, uid := range c.Srv.St.MemberIDs(ch.ID) {
			if !seen[uid] {
				seen[uid] = true
				c.Srv.PushState(uid)
			}
		}
	}
	c.Srv.PushState(c.User.ID)
	return nil
}

func cmdWhois(c *Ctx) error {
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	chans := c.Srv.userChannels(u.ID)
	names := make([]string, 0, len(chans))
	for _, ch := range chans {
		if ch.IsDM {
			continue
		}
		// Only channels the ASKER could have found anyway: a public one, or one
		// they are in themselves. /whois used to list every channel its subject
		// was in, invite-only rooms included — which turned it into a directory
		// of the private channels on the server.
		if ch.HasMode('i') {
			if _, err := c.Srv.requireMember(ch.ID, c.User.ID); err != nil {
				continue
			}
		}
		role, _ := c.Srv.St.MemberRole(ch.ID, u.ID)
		sigil := ""
		if role == "op" {
			sigil = "@"
		} else if role == "voice" {
			sigil = "+"
		}
		names = append(names, sigil+"#"+ch.Name)
	}
	online := "offline"
	if c.Srv.IsOnline(u.ID) {
		online = "online"
	}
	msg := fmt.Sprintf("whois %s\n  status: %s", u.Nick, online)
	if u.Away != "" {
		msg += "\n  away: " + u.Away
	}
	msg += "\n  channels: " + strings.Join(names, " ")
	c.Notice("%s", msg)
	return nil
}

func cmdList(c *Ctx) error {
	chans, err := c.Srv.St.PublicChannels()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d public channels:", len(chans)))
	for _, ch := range chans {
		b.WriteString(fmt.Sprintf("\n  #%-16s %3d  %s", ch["name"], ch["members"], ch["topic"]))
	}
	c.Notice("%s", b.String())
	return nil
}

func cmdMsg(c *Ctx) error {
	target := c.Arg(0)
	body := c.Rest(1)
	if target == "" {
		return errors.New("usage: /msg <nick> [text]")
	}
	if target == c.User.Nick {
		return errors.New("that would be talking to yourself")
	}
	u, err := c.Srv.St.UserByNick(target)
	if err != nil {
		return fmt.Errorf("no such user: %s", target)
	}
	ch, err := c.Srv.DMChannel(c.User, u)
	if err != nil {
		return err
	}
	// State first, then the message: the other side's client drops an event
	// for a channel it has not been told about, so a first DM posted before
	// the channel existed on their screen was read but never announced.
	c.Srv.PushState(c.User.ID)
	c.Srv.PushState(u.ID)
	// No text opens the conversation and says nothing — a click on @nick.
	if body != "" {
		return c.Srv.Post(ch, &Event{Kind: "message", Actor: c.User.Nick, Body: body}, "")
	}
	return nil
}

func cmdHelp(c *Ctx) error {
	if name := strings.TrimPrefix(c.Arg(0), "/"); name != "" {
		cmd, ok := c.Srv.Reg.Lookup(name)
		if !ok {
			return fmt.Errorf("no such command: /%s", name)
		}
		c.Notice("%s\n  %s\n  scope: %s · needs: %s · from: %s",
			cmd.Usage, cmd.Desc, cmd.Scope, cmd.MinRole, cmd.Source)
		return nil
	}
	var b strings.Builder
	b.WriteString("commands (the client palette is pushed from the server):")
	for _, cmd := range c.Srv.Reg.Manifest() {
		from := ""
		if cmd.Source != "builtin" {
			from = "  [" + cmd.Source + "]"
		}
		b.WriteString(fmt.Sprintf("\n  %-28s %s%s", cmd.Usage, cmd.Desc, from))
	}
	c.Notice("%s", b.String())
	return nil
}

// cmdUpdate only points: the updater is in the desktop shell, and it is this
// connection's shell that asked, so only this connection is told.
func cmdUpdate(c *Ctx) error {
	c.Conn.Send(Frame{T: "client.update", D: map[string]any{}})
	return nil
}

func cmdStatus(c *Ctx) error {
	client := c.Conn.client
	if client == "" {
		client = "unknown"
	}
	c.Srv.mu.RLock()
	users, conns := len(c.Srv.conns), 0
	for _, cs := range c.Srv.conns {
		conns += len(cs)
	}
	c.Srv.mu.RUnlock()
	c.Notice("server  %s %s, up %s\nclient  %s\nonline  %d users on %d connections",
		c.Srv.Name, c.Srv.Version, time.Since(c.Srv.Started).Round(time.Minute), client, users, conns)
	return nil
}

func cmdPlugins(c *Ctx) error {
	list := c.Srv.ConnectedPlugins()
	if len(list) == 0 {
		c.Notice("no plugins connected")
		return nil
	}
	var b strings.Builder
	b.WriteString("connected plugins:")
	for _, p := range list {
		b.WriteString(fmt.Sprintf("\n  %s (%s, id %s) — %d commands", p.Display, p.Kind, p.ID, p.NCommands))
	}
	c.Notice("%s", b.String())
	return nil
}

// cmdClientSide answers a client-side command that reached the server, which
// only an old client does: the work is on the device, never here.
func cmdClientSide(c *Ctx) error {
	return errors.New("that command runs in your client; update the app to use it")
}
