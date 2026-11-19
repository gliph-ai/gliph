package main

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// The stateful half of the retro pack: notify lists, /sound, CTCP pings,
// aliases and highlight words.
//
// Aliases and highlights live on the server on purpose. mIRC's lived on one
// machine and a reinstall took them with it; that loss is the whole reason to
// store them centrally now. The server only keeps the list — expansion and
// line highlighting stay client-side, where they belong.

const (
	retroKeyMax = 32  // an alias name or highlight word
	retroValMax = 400 // an alias expansion
)

// The retro tables and users.ctcp_ok cannot ride OpenStore's migration list
// (that file is owned elsewhere), so the schema is applied on first use of a
// database instead. Memoised per *Store so it costs one map lookup afterwards.
//
// ponytail: the map pins every *Store ever opened; a process opens one, and
// tests open a handful, so nothing is worth reclaiming here.
var retroMigrated sync.Map

func (s *Store) retroSchema() {
	if _, done := retroMigrated.Load(s); done {
		return
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS user_lists (
		   user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		   kind    TEXT NOT NULL,              -- alias | highlight | notify
		   name    TEXT NOT NULL,
		   val     TEXT NOT NULL DEFAULT '',
		   PRIMARY KEY (user_id, kind, name))`,
		// the reverse lookup: who is watching this user
		`CREATE INDEX IF NOT EXISTS user_lists_kind_name ON user_lists(kind, name)`,
		`ALTER TABLE users ADD COLUMN ctcp_ok INTEGER NOT NULL DEFAULT 1`,
	} {
		if _, err := s.db.DB.Exec(ddl(s.db.pg, stmt)); err != nil && !dupColumn(err) {
			log.Printf("retro migrate: %v", err)
		}
	}
	retroMigrated.Store(s, struct{}{})
}

// ---- registration ----

func registerRetroCommands(r *Registry) {
	add := func(name, usage, desc, scope, minRole string, args []Arg, h func(*Ctx) error) {
		r.AddBuiltin(&Command{Name: name, Usage: usage, Desc: desc, Scope: scope,
			MinRole: minRole, Args: args, Handler: h})
	}

	add("notify", "/notify [nick]", "watch for a friend coming online, or list your watches",
		"global", "member", []Arg{{Name: "nick", Type: "nick"}}, cmdNotify)

	add("unnotify", "/unnotify <nick>", "stop watching for someone", "global", "member",
		[]Arg{{Name: "nick", Type: "nick", Required: true}}, cmdUnnotify)

	add("sound", "/sound <name>", "play a named sound here — needs +s, once per 30s",
		"channel", "member", []Arg{{Name: "name", Type: "text", Required: true}}, cmdSound)

	add("ctcp", "/ctcp <nick> <version|time>", "ping someone's client, or /ctcp on|off for your own",
		"global", "member", []Arg{{Name: "nick", Type: "nick", Required: true},
			{Name: "query", Type: "text"}}, cmdCTCP)

	add("alias", "/alias <name> <expansion>", "save a slash alias, synced to all your devices",
		"global", "member", []Arg{{Name: "name", Type: "text", Required: true},
			{Name: "expansion", Type: "rest", Required: true}}, cmdAlias)

	add("aliases", "/aliases", "list your saved aliases", "global", "member", nil, cmdAliases)

	add("unalias", "/unalias <name>", "delete an alias", "global", "member",
		[]Arg{{Name: "name", Type: "text", Required: true}}, cmdUnalias)

	add("highlight", "/highlight <word>", "add a word that lights a line up for you",
		"global", "member", []Arg{{Name: "word", Type: "rest", Required: true}}, cmdHighlight)

	add("highlights", "/highlights", "list your highlight words", "global", "member", nil, cmdHighlights)

	add("unhighlight", "/unhighlight <word>", "remove a highlight word", "global", "member",
		[]Arg{{Name: "word", Type: "rest", Required: true}}, cmdUnhighlight)
}

// UserPrefs is the retro pack's slice of the state frame: everything a client
// needs to expand aliases and light up highlight words on its own.
func (s *Server) UserPrefs(uid string) map[string]any {
	return map[string]any{
		"aliases":    s.St.AliasesFor(uid),
		"highlights": s.St.HighlightsFor(uid),
		"ctcp_ok":    s.St.CtcpOK(uid),
	}
}

// cleanEntry validates one stored list entry. Control characters are rejected
// rather than stripped: they would corrupt the rendered line on every client,
// and silently mangling what someone typed is worse than saying no.
func cleanEntry(v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("that cannot be empty")
	}
	if len(v) > max {
		return "", fmt.Errorf("too long — max %d characters", max)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("control characters are not allowed")
	}
	return v, nil
}

// sortedKeys keeps every list the server ships in a stable order, so a client
// re-rendering after a sync does not reshuffle it.
func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

// ---- notify list ----

func cmdNotify(c *Ctx) error {
	if c.Arg(0) == "" {
		watched := c.Srv.St.NotifyList(c.User.ID)
		if len(watched) == 0 {
			return errors.New("your notify list is empty — /notify <nick>")
		}
		var b strings.Builder
		b.WriteString("notify list:")
		for _, u := range watched {
			mark := "·"
			if c.Srv.IsOnline(u.ID) {
				mark = "✓"
			}
			b.WriteString(fmt.Sprintf("\n  %s %s", mark, u.Nick))
		}
		c.Notice("%s", b.String())
		return nil
	}
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	if u.ID == c.User.ID {
		return errors.New("you are already online, from where you are sitting")
	}
	if err := c.Srv.St.AddNotify(c.User.ID, u.ID); err != nil {
		return err
	}
	// A watch is opt-in and never a secret: being watched is something you get
	// to know about, which is what keeps this friendly rather than creepy.
	c.Srv.SendToUser(u.ID, Frame{T: "notice", D: map[string]any{"channel": "", "level": "info",
		"text": fmt.Sprintf("%s added you to their notify list", c.User.Nick)}})
	c.Notice("watching %s — you will be told when they come online", u.Nick)
	return nil
}

func cmdUnnotify(c *Ctx) error {
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	if err := c.Srv.St.RemoveNotify(c.User.ID, u.ID); err != nil {
		return fmt.Errorf("you were not watching %s", u.Nick)
	}
	c.Notice("no longer watching %s", u.Nick)
	return nil
}

// NotifyOnline tells everyone watching uid that they just arrived. Call it from
// the connect path when a user goes from zero connections to one — a second
// device waking up is not news.
func (s *Server) NotifyOnline(userID string) {
	u, err := s.St.UserByID(userID)
	if err != nil {
		return
	}
	f := Frame{T: "notice", D: map[string]any{"channel": "", "level": "info",
		"text": fmt.Sprintf("✓ %s is online", u.Nick)}}
	for _, w := range s.St.watchersOf(userID) {
		s.SendToUser(w, f)
	}
}

// ---- /sound ----

var soundName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// One sound per user per channel per 30 seconds. Deliberately its own bucket
// rather than part of the shared flood budget: sounds are noise for everyone
// else in the room, so they get a limit tuned to patience, not to throughput.
//
// ponytail: keyed user|channel and never pruned; bounded by users times
// channels, which is small. Fold it into Limits.Prune if that stops being true.
var soundLimit = NewLimiter(1.0/30, 1)

func cmdSound(c *Ctx) error {
	if c.Channel == nil {
		return errors.New("/sound needs a channel")
	}
	if !c.Channel.HasMode('s') {
		return fmt.Errorf("#%s does not allow sounds — an op can /mode +s", c.Channel.Name)
	}
	name := strings.ToLower(c.Arg(0))
	if !soundName.MatchString(name) {
		return errors.New("a sound name is 1-32 characters of a-z, 0-9, - and _")
	}
	// charged only after validation, so a typo does not cost the budget
	if !soundLimit.Allow(c.User.ID + "|" + c.Channel.ID) {
		return errors.New("one sound per channel per 30 seconds — the 90s had limits too")
	}
	// Whether it actually makes a noise is the client's call: the per-user
	// opt-out lives there, so the server just states that a sound happened.
	return c.Srv.Post(c.Channel, &Event{Kind: "sound", Actor: c.User.Nick, Body: name,
		Meta: map[string]any{"sound": name}}, "")
}

// ---- CTCP ----

func cmdCTCP(c *Ctx) error {
	// one argument means you are talking about yourself, not pinging anyone
	if q := strings.ToLower(c.Arg(0)); c.Arg(1) == "" && (q == "on" || q == "off") {
		if err := c.Srv.St.SetCtcpOK(c.User.ID, q == "on"); err != nil {
			return err
		}
		c.Notice("your client answers CTCP pings: %s", q)
		return nil
	}
	u, err := c.Srv.St.UserByNick(c.Arg(0))
	if err != nil {
		return fmt.Errorf("no such user: %s", c.Arg(0))
	}
	if !c.Srv.St.CtcpOK(u.ID) {
		return fmt.Errorf("%s declines CTCP pings", u.Nick)
	}
	// Clients do not speak CTCP yet, so the server answers from what it already
	// knows. It never invents a client version it has not been told.
	switch strings.ToLower(c.Arg(1)) {
	case "time":
		c.Notice("CTCP TIME %s: %s (server time)", u.Nick, time.Now().UTC().Format(time.RFC1123))
	case "version":
		devs, err := c.Srv.St.DevicesForUser(u.ID)
		if err != nil {
			return err
		}
		if len(devs) == 0 {
			c.Notice("CTCP VERSION %s: no devices registered", u.Nick)
			return nil
		}
		names := make([]string, 0, len(devs))
		for _, d := range devs {
			if d.Name == "" {
				names = append(names, "unnamed")
				continue
			}
			names = append(names, d.Name)
		}
		c.Notice("CTCP VERSION %s: %d device(s) — %s", u.Nick, len(devs), strings.Join(names, ", "))
	default:
		return errors.New("usage: /ctcp <nick> <version|time>")
	}
	return nil
}

// ---- aliases ----

func cmdAlias(c *Ctx) error {
	name, err := cleanEntry(strings.ToLower(strings.TrimPrefix(c.Arg(0), "/")), retroKeyMax)
	if err != nil {
		return err
	}
	// An alias that shadowed /kick would be a trap, not a shortcut.
	if _, taken := c.Srv.Reg.Lookup(name); taken {
		return fmt.Errorf("/%s is already a command", name)
	}
	exp, err := cleanEntry(c.Rest(1), retroValMax)
	if err != nil {
		return err
	}
	if err := c.Srv.St.SetAlias(c.User.ID, name, exp); err != nil {
		return err
	}
	c.Srv.PushState(c.User.ID)
	c.Notice("/%s → %s", name, exp)
	return nil
}

func cmdAliases(c *Ctx) error {
	al := c.Srv.St.AliasesFor(c.User.ID)
	if len(al) == 0 {
		return errors.New("no aliases yet — /alias gm hello everyone")
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d aliases:", len(al)))
	for _, name := range sortedKeys(al) {
		b.WriteString(fmt.Sprintf("\n  /%-12s %s", name, al[name]))
	}
	c.Notice("%s", b.String())
	return nil
}

func cmdUnalias(c *Ctx) error {
	name := strings.ToLower(strings.TrimPrefix(c.Arg(0), "/"))
	if err := c.Srv.St.RemoveAlias(c.User.ID, name); err != nil {
		return err
	}
	c.Srv.PushState(c.User.ID)
	c.Notice("removed /%s", name)
	return nil
}

// ---- highlight words ----

func cmdHighlight(c *Ctx) error {
	// stored folded, because nobody wants to be pinged only in lower case
	word, err := cleanEntry(strings.ToLower(c.Raw), retroKeyMax)
	if err != nil {
		return err
	}
	if err := c.Srv.St.AddHighlight(c.User.ID, word); err != nil {
		return err
	}
	c.Srv.PushState(c.User.ID)
	c.Notice("highlighting %q", word)
	return nil
}

func cmdHighlights(c *Ctx) error {
	words := c.Srv.St.HighlightsFor(c.User.ID)
	if len(words) == 0 {
		return errors.New("no highlight words yet — /highlight <word>")
	}
	c.Notice("highlight words: %s", strings.Join(words, ", "))
	return nil
}

func cmdUnhighlight(c *Ctx) error {
	word := strings.ToLower(strings.TrimSpace(c.Raw))
	if err := c.Srv.St.RemoveHighlight(c.User.ID, word); err != nil {
		return err
	}
	c.Srv.PushState(c.User.ID)
	c.Notice("no longer highlighting %q", word)
	return nil
}
