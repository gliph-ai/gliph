package main

// The builtin blobs: todos, pins and check-ins. Each is a Provider plus a
// couple of slash commands, because the point of a blob is to be the browsing
// surface for something you can also do in one line from the composer.
//
// All three are `shared` mode: one document per channel that everyone sees,
// republished whenever it changes.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

func today() string { return time.Now().Format("2006-01-02") }

// ---- store ----

type Todo struct {
	ID       string
	Body     string
	Author   string
	Assignee string
	Done     bool
}

func (s *Store) AddTodo(cid, body, author string) (*Todo, error) {
	t := &Todo{ID: id("t"), Body: body, Author: author}
	_, err := s.db.Exec(`INSERT INTO todos(id,channel_id,body,author,created_at) VALUES(?,?,?,?,?)`,
		t.ID, cid, body, author, now())
	return t, err
}

func (s *Store) Todos(cid string) ([]Todo, error) {
	rows, err := s.db.Query(`SELECT id,body,author,assignee,done FROM todos
	  WHERE channel_id=? ORDER BY done, created_at`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Todo{}
	for rows.Next() {
		var t Todo
		var done int
		if err := rows.Scan(&t.ID, &t.Body, &t.Author, &t.Assignee, &done); err != nil {
			return nil, err
		}
		t.Done = done == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Todo(tid string) (*Todo, string, error) {
	var t Todo
	var done int
	var cid string
	err := s.db.QueryRow(`SELECT id,channel_id,body,author,assignee,done FROM todos WHERE id=?`, tid).
		Scan(&t.ID, &cid, &t.Body, &t.Author, &t.Assignee, &done)
	if err != nil {
		return nil, "", ErrNotFound
	}
	t.Done = done == 1
	return &t, cid, nil
}

func (s *Store) SetTodoDone(tid string, done bool) error {
	d, at := 0, ""
	if done {
		d, at = 1, now()
	}
	_, err := s.db.Exec(`UPDATE todos SET done=?, done_at=? WHERE id=?`, d, at, tid)
	return err
}

func (s *Store) SetTodoAssignee(tid, nick string) error {
	_, err := s.db.Exec(`UPDATE todos SET assignee=? WHERE id=?`, nick, tid)
	return err
}

func (s *Store) SetTodoBody(tid, body string) error {
	_, err := s.db.Exec(`UPDATE todos SET body=? WHERE id=?`, body, tid)
	return err
}

func (s *Store) DeleteTodo(tid string) error {
	_, err := s.db.Exec(`DELETE FROM todos WHERE id=?`, tid)
	return err
}

type Pin struct {
	ID      string
	Body    string
	Author  string
	EventID string
}

func (s *Store) AddPin(cid, body, author, eventID string) (*Pin, error) {
	p := &Pin{ID: id("p"), Body: body, Author: author, EventID: eventID}
	_, err := s.db.Exec(`INSERT INTO pins(id,channel_id,body,event_id,author,created_at) VALUES(?,?,?,?,?,?)`,
		p.ID, cid, body, eventID, author, now())
	return p, err
}

func (s *Store) Pins(cid string) ([]Pin, error) {
	rows, err := s.db.Query(`SELECT id,body,author,event_id FROM pins
	  WHERE channel_id=? ORDER BY created_at DESC`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pin{}
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.ID, &p.Body, &p.Author, &p.EventID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PinAuthor(pid string) (string, bool) {
	var a string
	if err := s.db.QueryRow(`SELECT author FROM pins WHERE id=?`, pid).Scan(&a); err != nil {
		return "", false
	}
	return a, true
}

func (s *Store) DeletePin(pid string) error {
	_, err := s.db.Exec(`DELETE FROM pins WHERE id=?`, pid)
	return err
}

func (s *Store) PutCheckin(cid, uid, body string) error {
	_, err := s.db.Exec(`INSERT INTO checkins(channel_id,user_id,day,body,ts) VALUES(?,?,?,?,?)
	  ON CONFLICT(channel_id,user_id,day) DO UPDATE SET body=excluded.body, ts=excluded.ts`,
		cid, uid, today(), body, now())
	return err
}

func (s *Store) ClearCheckin(cid, uid string) error {
	_, err := s.db.Exec(`DELETE FROM checkins WHERE channel_id=? AND user_id=? AND day=?`, cid, uid, today())
	return err
}

type Checkin struct {
	Nick string
	Body string
	TS   string
}

func (s *Store) Checkins(cid, day string) ([]Checkin, error) {
	rows, err := s.db.Query(`SELECT u.nick, c.body, c.ts FROM checkins c
	  JOIN users u ON u.id=c.user_id WHERE c.channel_id=? AND c.day=? ORDER BY c.ts`, cid, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkin{}
	for rows.Next() {
		var c Checkin
		if err := rows.Scan(&c.Nick, &c.Body, &c.TS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- todos blob ----

type todosBlob struct{}

func (todosBlob) Open(ctx *BlobCtx) (*Doc, error) { return todosDoc(ctx.Srv, ctx.Channel.ID) }

func todosDoc(s *Server, cid string) (*Doc, error) {
	items, err := s.St.Todos(cid)
	if err != nil {
		return nil, err
	}
	open, done := []Item{}, []Item{}
	for _, t := range items {
		hint := "by " + t.Author
		if t.Assignee != "" {
			hint += " · @" + t.Assignee
		}
		it := Item{ID: t.ID, Label: t.Body, Hint: hint, State: "open", Actions: []Action{
			{ID: "toggle", Label: "mark done"},
			{ID: "assign", Label: "assign to…"},
			{ID: "edit", Label: "edit", Prompt: "new text"},
			{ID: "delete", Label: "delete", Confirm: "delete this todo?"},
		}}
		if t.Done {
			it.State = "done"
			it.Actions = []Action{
				{ID: "toggle", Label: "reopen"},
				{ID: "delete", Label: "delete", Confirm: "delete this todo?"},
			}
			done = append(done, it)
			continue
		}
		open = append(open, it)
	}
	doc := &Doc{Title: "todos", Blocks: []Block{
		text(fmt.Sprintf("%d open · %d done", len(open), len(done)), "info"),
	}}
	if len(open) > 0 {
		doc.Blocks = append(doc.Blocks, list("open", open))
	} else {
		doc.Blocks = append(doc.Blocks, text("nothing open. add one below.", "plain"))
	}
	if len(done) > 0 {
		if len(done) > 20 {
			done = done[:20]
		}
		doc.Blocks = append(doc.Blocks, divider("done"), list("done", done))
	}
	doc.Blocks = append(doc.Blocks,
		input("new", "add", "what needs doing?", "add"),
		bar("enter: menu · esc: back to chat"))
	return doc, nil
}

func (todosBlob) Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error) {
	s, cid := ctx.Srv, ctx.Channel.ID
	switch ev.Action {
	case "add":
		body := clean(ev.Value, 500)
		if body == "" {
			return nil, nil, errors.New("nothing to add")
		}
		if _, err := s.St.AddTodo(cid, body, ctx.User.Nick); err != nil {
			return nil, nil, err
		}
		s.Post(ctx.Channel, &Event{Kind: "notice", Actor: ctx.User.Nick,
			Body: "added a todo: " + body}, "")

	case "toggle":
		t, tcid, err := s.St.Todo(ev.Item)
		if err != nil || tcid != cid {
			return nil, nil, errors.New("no such todo")
		}
		if err := s.St.SetTodoDone(t.ID, !t.Done); err != nil {
			return nil, nil, err
		}
		if !t.Done {
			s.Post(ctx.Channel, &Event{Kind: "notice", Actor: ctx.User.Nick,
				Body: "completed: " + t.Body}, "")
		}

	case "assign":
		// A submenu of the channel's members: the provider decides what the
		// choices are, the client just renders them.
		members, _ := s.St.Members(cid)
		acts := []Action{{ID: "assign:", Label: "nobody"}}
		for _, m := range members {
			if m.Kind != "user" {
				continue
			}
			acts = append(acts, Action{ID: "assign:" + m.Nick, Label: m.Nick})
		}
		return nil, &Menu{Item: ev.Item, Actions: acts}, nil

	case "edit":
		body := clean(ev.Value, 500)
		if body == "" {
			return nil, nil, errors.New("nothing to save")
		}
		if _, tcid, err := s.St.Todo(ev.Item); err != nil || tcid != cid {
			return nil, nil, errors.New("no such todo")
		}
		if err := s.St.SetTodoBody(ev.Item, body); err != nil {
			return nil, nil, err
		}

	case "delete":
		t, tcid, err := s.St.Todo(ev.Item)
		if err != nil || tcid != cid {
			return nil, nil, errors.New("no such todo")
		}
		// author or op may delete
		if t.Author != ctx.User.Nick && ctx.Role != "op" && ctx.User.Role != "admin" {
			return nil, nil, errors.New("only the author or an op can delete that")
		}
		if err := s.St.DeleteTodo(t.ID); err != nil {
			return nil, nil, err
		}

	default:
		if nick, ok := strings.CutPrefix(ev.Action, "assign:"); ok {
			if _, tcid, err := s.St.Todo(ev.Item); err != nil || tcid != cid {
				return nil, nil, errors.New("no such todo")
			}
			if err := s.St.SetTodoAssignee(ev.Item, nick); err != nil {
				return nil, nil, err
			}
			break
		}
		return nil, nil, fmt.Errorf("unknown action %q", ev.Action)
	}
	doc, err := todosDoc(s, cid)
	return doc, nil, err
}

// ---- pins blob ----

type pinsBlob struct{}

func (pinsBlob) Open(ctx *BlobCtx) (*Doc, error) { return pinsDoc(ctx.Srv, ctx.Channel.ID) }

func pinsDoc(s *Server, cid string) (*Doc, error) {
	pins, err := s.St.Pins(cid)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(pins))
	for _, p := range pins {
		items = append(items, Item{ID: p.ID, Label: p.Body, Hint: "by " + p.Author,
			Actions: []Action{{ID: "unpin", Label: "unpin", Confirm: "unpin this?"}}})
	}
	doc := &Doc{Title: "pins", Blocks: []Block{
		text(fmt.Sprintf("%d pinned", len(items)), "info"),
	}}
	if len(items) > 0 {
		doc.Blocks = append(doc.Blocks, list("pins", items))
	} else {
		doc.Blocks = append(doc.Blocks, text("nothing pinned yet.", "plain"))
	}
	doc.Blocks = append(doc.Blocks,
		input("new", "pin", "a link or a note", "add"),
		bar("enter: menu · esc: back to chat"))
	return doc, nil
}

func (pinsBlob) Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error) {
	s, cid := ctx.Srv, ctx.Channel.ID
	switch ev.Action {
	case "add":
		body := clean(ev.Value, 1000)
		if body == "" {
			return nil, nil, errors.New("nothing to pin")
		}
		if _, err := s.St.AddPin(cid, body, ctx.User.Nick, ""); err != nil {
			return nil, nil, err
		}
		s.Post(ctx.Channel, &Event{Kind: "notice", Actor: ctx.User.Nick, Body: "pinned: " + body}, "")
	case "unpin":
		author, ok := s.St.PinAuthor(ev.Item)
		if !ok {
			return nil, nil, errors.New("no such pin")
		}
		if author != ctx.User.Nick && ctx.Role != "op" && ctx.User.Role != "admin" {
			return nil, nil, errors.New("only the pinner or an op can unpin that")
		}
		if err := s.St.DeletePin(ev.Item); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("unknown action %q", ev.Action)
	}
	doc, err := pinsDoc(s, cid)
	return doc, nil, err
}

// ---- check-ins blob ----

type checkinsBlob struct{}

func (checkinsBlob) Open(ctx *BlobCtx) (*Doc, error) { return checkinsDoc(ctx.Srv, ctx.Channel.ID) }

func checkinsDoc(s *Server, cid string) (*Doc, error) {
	ins, err := s.St.Checkins(cid, today())
	if err != nil {
		return nil, err
	}
	said := map[string]bool{}
	items := make([]Item, 0, len(ins))
	for _, c := range ins {
		said[c.Nick] = true
		items = append(items, Item{ID: c.Nick, Label: c.Nick, Hint: c.Body, State: "ok",
			Actions: []Action{
				{ID: "edit", Label: "edit mine", Prompt: "what are you working on?"},
				{ID: "clear", Label: "clear mine", Confirm: "clear your check-in?"},
			}})
	}
	members, _ := s.St.Members(cid)
	quiet := []string{}
	for _, m := range members {
		if m.Kind == "user" && !said[m.Nick] {
			quiet = append(quiet, m.Nick)
		}
	}
	sort.Strings(quiet)

	doc := &Doc{Title: "check-ins", Blocks: []Block{
		text(fmt.Sprintf("%s · %d of %d checked in", today(), len(items), len(items)+len(quiet)), "info"),
	}}
	if len(items) > 0 {
		doc.Blocks = append(doc.Blocks, list("today", items))
	} else {
		doc.Blocks = append(doc.Blocks, text("nobody has checked in yet today.", "plain"))
	}
	if len(quiet) > 0 {
		doc.Blocks = append(doc.Blocks, divider("not yet"),
			text(strings.Join(quiet, " · "), "warn"),
			buttons(Action{ID: "remind", Label: "remind them", MinRole: "op",
				Confirm: "post a reminder to the channel?"}))
	}
	doc.Blocks = append(doc.Blocks,
		input("mine", "check in", "what are you working on today?", "checkin"),
		bar("enter: menu · esc: back to chat"))
	return doc, nil
}

func (checkinsBlob) Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error) {
	s, cid := ctx.Srv, ctx.Channel.ID
	switch ev.Action {
	case "checkin", "edit":
		body := clean(ev.Value, 300)
		if body == "" {
			return nil, nil, errors.New("say what you are working on")
		}
		if err := s.St.PutCheckin(cid, ctx.User.ID, body); err != nil {
			return nil, nil, err
		}
		// The channel log is the history; the blob is just today, organised.
		s.Post(ctx.Channel, &Event{Kind: "notice", Actor: ctx.User.Nick,
			Body: "checked in: " + body}, "")
	case "clear":
		if err := s.St.ClearCheckin(cid, ctx.User.ID); err != nil {
			return nil, nil, err
		}
	case "remind":
		ins, _ := s.St.Checkins(cid, today())
		said := map[string]bool{}
		for _, c := range ins {
			said[c.Nick] = true
		}
		members, _ := s.St.Members(cid)
		quiet := []string{}
		for _, m := range members {
			if m.Kind == "user" && !said[m.Nick] {
				quiet = append(quiet, m.Nick)
			}
		}
		if len(quiet) == 0 {
			return nil, nil, errors.New("everyone has checked in")
		}
		s.Post(ctx.Channel, &Event{Kind: "notice", Actor: ctx.User.Nick,
			Body: "still to check in: " + strings.Join(quiet, ", ")}, "")
	default:
		return nil, nil, fmt.Errorf("unknown action %q", ev.Action)
	}
	doc, err := checkinsDoc(s, cid)
	return doc, nil, err
}

// ---- registration ----

func registerBuiltinBlobs(s *Server) {
	s.Blobs.AddBuiltin("todos", "todos", "shared", todosBlob{})
	s.Blobs.AddBuiltin("pins", "pins", "shared", pinsBlob{})
	s.Blobs.AddBuiltin("checkins", "check-ins", "shared", checkinsBlob{})
	s.Blobs.AddBuiltin("devlog", "devlog", "private", devlogBlob{}) // per-viewer filter — see devlog.go
}

// The slash commands that pair with the blobs: a blob is the browsing surface,
// but you should never have to open it to add one line.
func registerBlobCommands(r *Registry) {
	r.AddBuiltin(&Command{
		Name: "todo", Usage: "/todo <text> | done <n>", Desc: "add a todo, or tick one off",
		Scope: "channel", MinRole: "member",
		Args:    []Arg{{Name: "text", Type: "rest", Required: true}},
		Handler: cmdTodo,
	})
	r.AddBuiltin(&Command{
		Name: "pin", Usage: "/pin [text or url]", Desc: "pin a note, or the last message",
		Scope: "channel", MinRole: "member",
		Args:    []Arg{{Name: "text", Type: "rest"}},
		Handler: cmdPin,
	})
	checkin := &Command{
		Name: "checkin", Usage: "/checkin [what you are doing]",
		Desc:  "today's check-in, or the board if you say nothing",
		Scope: "channel", MinRole: "member",
		Args:    []Arg{{Name: "text", Type: "rest"}},
		Handler: cmdCheckin,
	}
	r.AddBuiltin(checkin)
	// The same command by the name half the world calls it. An alias rather
	// than a second implementation: `/aliases` is per-user, and this one should
	// be there for everybody on every server.
	standup := *checkin
	standup.Name, standup.Usage = "standup", "/standup [what you are doing]"
	r.AddBuiltin(&standup)

	// The blob is the browsing surface, so opening it is worth one word.
	for _, b := range []struct{ cmd, blob, desc string }{
		{"todos", "todos", "open this channel's todo list"},
		{"pins", "pins", "open this channel's pins"},
	} {
		b := b
		r.AddBuiltin(&Command{
			Name: b.cmd, Usage: "/" + b.cmd, Desc: b.desc,
			Scope: "channel", MinRole: "member",
			Handler: func(c *Ctx) error { return c.FocusBlob(b.blob) },
		})
	}
	r.AddBuiltin(&Command{
		Name: "blobs", Usage: "/blobs", Desc: "list the apps attached to this channel",
		Scope: "channel", MinRole: "member",
		Handler: cmdBlobs,
	})

	registerDevlogCommands(r) // /log and /hook — see devlog.go
}

func cmdBlobs(c *Ctx) error {
	specs := c.Srv.Blobs.For(c.Srv, c.Channel)
	if len(specs) == 0 {
		c.Notice("no apps in this channel")
		return nil
	}
	parts := make([]string, 0, len(specs))
	for _, sp := range specs {
		// Where a blob came from is worth showing: a plugin's apps come and go
		// with the plugin, exactly as its commands do.
		if sp.Source == "builtin" {
			parts = append(parts, sp.ID)
		} else {
			parts = append(parts, sp.ID+" (from "+sp.Source+")")
		}
	}
	// Deliberately not "/<id> opens one": only some blobs have a command of
	// their own, and naming one that does not exist is worse than naming none.
	c.Notice("apps here: %s — pick one from the switcher, or ctrl+1..9",
		strings.Join(parts, " · "))
	return nil
}

func cmdTodo(c *Ctx) error {
	body := clean(c.Raw, 500)
	if body == "" {
		return errors.New("usage: /todo <text>, or /todo done <n>")
	}
	// `/todo done 3` ticks off the third OPEN item — the number the blob shows
	// against it. Without this branch it silently added a todo called "done 3",
	// which is worse than refusing.
	if rest, ok := strings.CutPrefix(body, "done "); ok {
		return todoDone(c, strings.TrimSpace(rest))
	}
	if _, err := c.Srv.St.AddTodo(c.Channel.ID, body, c.User.Nick); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick, Body: "added a todo: " + body}, "")
	c.Srv.refreshBlob(c.Channel.ID, "todos")
	return nil
}

func todoDone(c *Ctx, arg string) error {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		return errors.New("usage: /todo done <n> — the number shown in /todos")
	}
	items, err := c.Srv.St.Todos(c.Channel.ID)
	if err != nil {
		return err
	}
	open := make([]Todo, 0, len(items))
	for _, t := range items {
		if !t.Done {
			open = append(open, t)
		}
	}
	if n > len(open) {
		return fmt.Errorf("there %s only %d open todo%s",
			map[bool]string{true: "is", false: "are"}[len(open) == 1], len(open),
			map[bool]string{true: "", false: "s"}[len(open) == 1])
	}
	t := open[n-1]
	if err := c.Srv.St.SetTodoDone(t.ID, true); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick, Body: "completed: " + t.Body}, "")
	c.Srv.refreshBlob(c.Channel.ID, "todos")
	return nil
}

func cmdPin(c *Ctx) error {
	body, eventID := clean(c.Raw, 1000), ""
	// Bare `/pin` pins the last thing said, which is what you almost always
	// mean. The plan wanted `/pin <seq>` addressing a message by a visible
	// number, but no client renders one — so this is the same gesture without
	// inventing an index for people to read off the screen.
	if body == "" {
		last, err := c.Srv.St.LastMessage(c.Channel.ID)
		if err != nil || last == nil {
			return errors.New("nothing to pin yet — /pin <text or url> to pin a note")
		}
		if last.Enc {
			// The server holds ciphertext for a +e channel and must not store a
			// second copy of it under another name.
			return errors.New("cannot pin a message in an encrypted channel — /pin <text> instead")
		}
		body, eventID = "<"+last.Actor+"> "+clean(last.Body, 1000), last.ID
	}
	if _, err := c.Srv.St.AddPin(c.Channel.ID, body, c.User.Nick, eventID); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick, Body: "pinned: " + body}, "")
	c.Srv.refreshBlob(c.Channel.ID, "pins")
	return nil
}

func cmdCheckin(c *Ctx) error {
	body := clean(c.Raw, 300)
	// Bare `/checkin` reads the board rather than complaining: asking "who has
	// checked in?" is the other half of the same word.
	if body == "" {
		return checkinBoard(c)
	}
	if err := c.Srv.St.PutCheckin(c.Channel.ID, c.User.ID, body); err != nil {
		return err
	}
	c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick, Body: "checked in: " + body}, "")
	c.Srv.refreshBlob(c.Channel.ID, "checkins")
	return nil
}

// checkinBoard answers a bare /checkin with today's board as a notice, so the
// question can be asked without leaving the composer.
func checkinBoard(c *Ctx) error {
	ins, err := c.Srv.St.Checkins(c.Channel.ID, today())
	if err != nil {
		return err
	}
	said := map[string]bool{}
	lines := make([]string, 0, len(ins))
	for _, in := range ins {
		said[in.Nick] = true
		lines = append(lines, in.Nick+": "+in.Body)
	}
	members, _ := c.Srv.St.Members(c.Channel.ID)
	quiet := []string{}
	for _, m := range members {
		if m.Kind == "user" && !said[m.Nick] {
			quiet = append(quiet, m.Nick)
		}
	}
	sort.Strings(quiet)
	if len(lines) == 0 {
		c.Notice("nobody has checked in today — /checkin <what you are working on>")
		return nil
	}
	c.Notice("%s — %s", today(), strings.Join(lines, " · "))
	if len(quiet) > 0 {
		c.Notice("not yet: %s", strings.Join(quiet, " · "))
	}
	return nil
}

// refreshBlob re-renders a shared blob after something changed it from outside
// (a slash command), so anyone with it open sees the change immediately.
func (s *Server) refreshBlob(cid, blobID string) {
	sp, ok := s.Blobs.Lookup(blobID)
	if !ok || sp.Mode != "shared" || sp.Source != "builtin" {
		return
	}
	ch, err := s.St.ChannelByID(cid)
	if err != nil {
		return
	}
	doc, err := sp.provider.Open(&BlobCtx{Srv: s, Channel: ch})
	if err != nil || doc == nil {
		return
	}
	s.PublishBlob(cid, blobID, doc)
}
