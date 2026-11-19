package main

// The devlog: git pushes and hand-written log lines. There is deliberately no
// devlog table — a push is an ordinary channel event, so scrollback, offline
// cache, search and the flat log all work on commits for free. The blob is
// only a lens over the channel's `commit` and `log` events.
//
// The webhook is the security-critical half. The token in the URL is also the
// shared secret, so every delivery is checked twice: the token must resolve to
// a channel, and the payload must carry a signature made with that same token
// (GitHub: HMAC-SHA256 over the raw body; GitLab: a plain token header). Both
// comparisons are constant-time and a failure is a 403 that never touches the
// channel. Revoking the row kills the URL and the secret in one move.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// devlogMax caps what the blob reads back. The blob is a lens on recent
	// work, not an archive — the channel log itself is the archive.
	devlogMax = 200
	// hookBodyMax: a GitHub push payload with the maximum 20 commits is a few
	// tens of KB. A megabyte is generous and bounds an unauthenticated read.
	hookBodyMax = 1 << 20
	// hookCommitMax bounds one event's meta so a 1000-commit force push cannot
	// write an enormous row. The summary line still reports the true count.
	hookCommitMax = 50
	// summaryCommits is how many subjects the one-line body names before "…".
	summaryCommits = 3
)

// ---- store ----

type Hook struct {
	ID      string
	Channel string
	Token   string
	Kind    string // git | in | out
	Name    string // in: the label its lines post under
	URL     string // out: where deliveries go
	By      string
	Created string
}

const hookCols = `id,channel_id,token,kind,name,url,created_by,created_at`

func scanHook(sc interface{ Scan(...any) error }, h *Hook) error {
	return sc.Scan(&h.ID, &h.Channel, &h.Token, &h.Kind, &h.Name, &h.URL, &h.By, &h.Created)
}

func (s *Store) CreateHook(cid, kind, by string) (*Hook, error) {
	return s.CreateWebhook(cid, kind, "", "", by)
}

func (s *Store) CreateWebhook(cid, kind, name, url, by string) (*Hook, error) {
	h := &Hook{ID: id("h"), Channel: cid, Token: token(), Kind: kind, Name: name, URL: url, By: by, Created: now()}
	_, err := s.db.Exec(`INSERT INTO hooks(`+hookCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		h.ID, cid, h.Token, kind, name, url, by, h.Created)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (s *Store) Hooks(cid string) ([]Hook, error) {
	rows, err := s.db.Query(`SELECT `+hookCols+` FROM hooks
	  WHERE channel_id=? ORDER BY created_at`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hook{}
	for rows.Next() {
		var h Hook
		if err := scanHook(rows, &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) HookByToken(t string) (*Hook, error) {
	var h Hook
	if t == "" {
		return nil, ErrNotFound
	}
	err := scanHook(s.db.QueryRow(`SELECT `+hookCols+` FROM hooks WHERE token=?`, t), &h)
	if err != nil {
		return nil, ErrNotFound
	}
	return &h, nil
}

// DeleteHook is scoped to the channel: a hook id is not a secret, so an op in
// one channel must not be able to revoke another channel's hook by guessing.
func (s *Store) DeleteHook(cid, hid string) error {
	res, err := s.db.Exec(`DELETE FROM hooks WHERE id=? AND channel_id=?`, hid, cid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such hook here: %s", hid)
	}
	return nil
}

// DevlogEvents is the blob's whole query: the channel's commits and log lines,
// newest first, capped.
func (s *Store) DevlogEvents(cid string, limit int) ([]*Event, error) {
	// `kind` has no index, so in a channel with few commits this walks back
	// through the channel's history. On the read pool that costs the caller
	// time; on the writer it would cost everyone.
	rows, err := s.r.Query(`SELECT id,seq,kind,actor,ts,body,meta FROM events
	  WHERE channel_id=? AND kind IN ('commit','log') ORDER BY seq DESC LIMIT ?`, cid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		e := &Event{Channel: cid}
		var mj string
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.Actor, &e.TS, &e.Body, &mj); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(mj), &e.Meta)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- webhook ----

// DevlogRoutes registers the git webhook endpoint. It lives here rather than in
// Routes() so the whole feature is one file; Routes() only has to call it.
func (s *Server) DevlogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/hooks/git/{token}", s.handleGitHook)
	mux.HandleFunc("POST /api/hooks/in/{token}", s.handleInHook) // webhooks.go
}

// githubSigned checks GitHub's X-Hub-Signature-256 over the exact bytes we
// read. Recomputing from a re-encoded payload would be a subtle way to accept
// a forgery, so the raw body is the only thing ever hashed.
func githubSigned(body []byte, sig, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

func (s *Server) handleGitHook(w http.ResponseWriter, r *http.Request) {
	h, err := s.St.HookByToken(r.PathValue("token"))
	if err == nil && h.Kind != "git" {
		err = ErrNotFound // a token is scoped to its own endpoint
	}
	if err != nil {
		// Same answer for an unknown and a revoked token: nothing to learn here.
		fail(w, 403, "unknown or revoked hook")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, hookBodyMax))
	if err != nil {
		fail(w, 400, "unreadable body")
		return
	}

	// Signature first, always — including for events we are about to drop, so
	// an unverified caller can never tell a push from a ping by the response.
	var push bool
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		if !githubSigned(body, r.Header.Get("X-Hub-Signature-256"), h.Token) {
			fail(w, 403, "bad signature")
			return
		}
		push = r.Header.Get("X-GitHub-Event") == "push"
	case r.Header.Get("X-Gitlab-Event") != "":
		// GitLab has no HMAC, only a shared token. Compared in constant time
		// all the same: a byte-by-byte == would leak the prefix by timing.
		if !hmac.Equal([]byte(r.Header.Get("X-Gitlab-Token")), []byte(h.Token)) {
			fail(w, 403, "bad token")
			return
		}
		push = r.Header.Get("X-Gitlab-Event") == "Push Hook"
	default:
		fail(w, 400, "not a GitHub or GitLab webhook")
		return
	}

	// Anything that is not a push (ping, tag, merge request, issue) is accepted
	// and dropped. A 200 tells the provider we got it and stops it retrying
	// forever something we are never going to want.
	if !push {
		writeJSON(w, 200, map[string]any{"ok": true, "ignored": true})
		return
	}

	ev, err := gitPushEvent(body)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ch, err := s.St.ChannelByID(h.Channel)
	if err != nil {
		fail(w, 404, "the hook's channel is gone")
		return
	}
	if err := s.Post(ch, ev, ""); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pushPayload is GitHub's and GitLab's push bodies in one struct: the fields
// they do not share simply stay empty, which is cheaper than two decoders and
// a branch for a difference that is three field names wide.
type pushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		FullName string `json:"full_name"` // GitHub
		Name     string `json:"name"`
	} `json:"repository"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"` // GitLab
	} `json:"project"`
	Pusher struct {
		Name string `json:"name"` // GitHub
	} `json:"pusher"`
	UserUsername string `json:"user_username"` // GitLab
	UserName     string `json:"user_name"`
	Commits      []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
		URL     string `json:"url"`
	} `json:"commits"`
}

// gitPushEvent turns one push into one event — not one per commit. A branch
// with fifty commits is one thing that happened, and fifty lines would bury
// the conversation it lands next to.
func gitPushEvent(payload []byte) (*Event, error) {
	var p pushPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, errors.New("malformed push payload")
	}
	repo := firstNonEmpty(p.Project.PathWithNamespace, p.Repository.FullName, p.Repository.Name, "unknown")
	branch := strings.TrimPrefix(strings.TrimPrefix(p.Ref, "refs/heads/"), "refs/tags/")
	pusher := firstNonEmpty(p.Pusher.Name, p.UserUsername, p.UserName, "someone")

	commits := make([]map[string]string, 0, len(p.Commits))
	subjects := make([]string, 0, len(p.Commits))
	for _, c := range p.Commits {
		subject := clean(firstLine(c.Message), 200)
		if len(commits) < hookCommitMax {
			commits = append(commits, map[string]string{
				"id": clean(c.ID, 64), "msg": subject, "url": clean(c.URL, 500)})
		}
		if len(subjects) < summaryCommits {
			subjects = append(subjects, subject)
		}
	}

	// "push to gliph/main by waseem" — the repo's own name, not owner/repo,
	// because the owner is the same for every push a channel ever sees.
	short := repo
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	body := fmt.Sprintf("push to %s/%s by %s", clean(short, 100), clean(branch, 100), clean(pusher, 100))
	switch n := len(p.Commits); {
	case n == 0:
		// A branch delete (or a tag push) carries no commits. It is still news.
		body += " — no commits (branch deleted or tag pushed)"
	default:
		body += fmt.Sprintf(" — %d commit%s: %s", n, plural(n), strings.Join(subjects, "; "))
		if n > len(subjects) {
			body += "; …"
		}
	}
	return &Event{Kind: "commit", Actor: clean(pusher, 100), Body: body,
		Meta: map[string]any{"repo": repo, "branch": branch, "pusher": pusher, "commits": commits}}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- devlog blob ----
//
// Mode is `private`, not `shared`, and the filter is why. A shared blob has one
// document per channel, so one person typing "auth" into the filter would
// re-render everyone else's screen — per-viewer state cannot live in a document
// everyone shares. The alternatives were to drop the filter or to go private;
// private wins because the devlog has no shared mutable state to keep in sync
// (it is a read-only projection of the event log), so a per-viewer render costs
// nothing and is always fresh on open, where a shared doc would serve whatever
// was last published until something republished it.

type devlogBlob struct{}

func (devlogBlob) Open(ctx *BlobCtx) (*Doc, error) {
	return devlogDoc(ctx.Srv, ctx.Channel.ID, "")
}

func metaStr(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// headCommitURL is the last commit in the push — the one the branch now points
// at, and the one you want when you click a row.
func headCommitURL(m map[string]any) string {
	cs, _ := m["commits"].([]any)
	for i := len(cs) - 1; i >= 0; i-- {
		c, ok := cs[i].(map[string]any)
		if !ok {
			continue
		}
		if u, _ := c["url"].(string); u != "" {
			return u
		}
	}
	return ""
}

func devlogDoc(s *Server, cid, filter string) (*Doc, error) {
	evs, err := s.St.DevlogEvents(cid, devlogMax)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(filter)
	items := make([]Item, 0, len(evs))
	for _, e := range evs {
		hint, state := "log · "+e.Actor, "ok"
		var actions []Action
		if e.Kind == "commit" {
			repo, branch := metaStr(e.Meta, "repo"), metaStr(e.Meta, "branch")
			hint = strings.Join([]string{repo, branch, metaStr(e.Meta, "pusher")}, " · ")
			state = ""
			// The action id carries the URL because GUM actions carry no
			// payload of their own — same trick as todos' "assign:<nick>".
			if u := headCommitURL(e.Meta); u != "" {
				actions = append(actions, Action{ID: "open:" + u, Label: "open"})
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Body+" "+hint), needle) {
			continue
		}
		items = append(items, Item{ID: e.ID, Label: e.Body, Hint: hint, State: state, Actions: actions})
	}

	head := fmt.Sprintf("%d entries", len(items))
	if filter != "" {
		head = fmt.Sprintf("%d of %d entries matching %q", len(items), len(evs), filter)
	}
	doc := &Doc{Title: "devlog", Blocks: []Block{text(head, "info")}}
	switch {
	case len(items) > 0:
		doc.Blocks = append(doc.Blocks, list("entries", items))
	case filter != "":
		doc.Blocks = append(doc.Blocks, text("nothing matches that filter.", "plain"))
	default:
		doc.Blocks = append(doc.Blocks,
			text("no commits or log entries yet. /hook new wires up git; /log writes a line by hand.", "plain"))
	}
	doc.Blocks = append(doc.Blocks,
		input("filter", "filter", "repo, branch, author or text", "filter"),
		buttons(Action{ID: "clear", Label: "clear filter"}),
		bar("enter: menu · esc: back to chat"))
	return doc, nil
}

func (devlogBlob) Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error) {
	switch {
	case ev.Action == "filter":
		doc, err := devlogDoc(ctx.Srv, ctx.Channel.ID, clean(ev.Value, 100))
		return doc, nil, err
	case ev.Action == "clear":
		doc, err := devlogDoc(ctx.Srv, ctx.Channel.ID, "")
		return doc, nil, err
	case strings.HasPrefix(ev.Action, "open:"):
		// A server cannot open a browser, so it hands the URL back as a notice
		// and the client does whatever it does with links. Leaving the document
		// alone keeps the reader's place in the list.
		ctx.Conn.Send(Frame{T: "notice", D: map[string]any{
			"channel": ctx.Channel.ID, "level": "info",
			"text": strings.TrimPrefix(ev.Action, "open:")}})
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unknown action %q", ev.Action)
}

// ---- commands ----

// registerDevlogCommands is called from registerBlobCommands, so the devlog
// registers itself the same way the other builtin apps do.
func registerDevlogCommands(r *Registry) {
	r.AddBuiltin(&Command{
		Name: "log", Usage: "/log <text>", Desc: "add a devlog entry to this channel",
		Scope: "channel", MinRole: "member",
		Args:    []Arg{{Name: "text", Type: "rest", Required: true}},
		Handler: cmdLog,
	})
	// Op for every subcommand, list included: a hook is a write channel into
	// the room, and who has one is operator business.
	r.AddBuiltin(&Command{
		Name: "hook", Usage: "/hook new|in <name>|out <url>|list|revoke <id>", Desc: "git, incoming and outgoing webhooks for this channel",
		Scope: "channel", MinRole: "op",
		Args: []Arg{{Name: "action", Type: "text", Required: true},
			{Name: "id", Type: "text"}},
		Handler: cmdHook,
	})
}

func cmdLog(c *Ctx) error {
	body := clean(c.Raw, 1000)
	if body == "" {
		return errors.New("usage: /log <text>")
	}
	return c.Srv.Post(c.Channel, &Event{Kind: "log", Actor: c.User.Nick, Body: body}, "")
}

// hookURL builds what an operator pastes into GitHub or GitLab. The server has
// no configured public URL; -name is the closest thing it has and is already
// what clients are told to call it, so an operator behind a different hostname
// edits the host and keeps the path.
func hookURL(s *Server, tok string) string {
	return "https://" + s.Name + "/api/hooks/git/" + tok
}

func cmdHook(c *Ctx) error {
	switch strings.ToLower(c.Arg(0)) {
	case "new":
		h, err := c.Srv.St.CreateHook(c.Channel.ID, "git", c.User.Nick)
		if err != nil {
			return err
		}
		c.Notice("hook %s created for #%s", h.ID, c.Channel.Name)
		c.Notice("%s", hookURL(c.Srv, h.Token))
		c.Notice("paste it as the payload URL *and* as the secret (GitHub) or secret token (GitLab)")
		c.Notice("treat it as a password — anyone holding it can post here. revoke: /hook revoke %s", h.ID)

	case "in", "out":
		return cmdWebhook(c) // webhooks.go

	case "list":
		hooks, err := c.Srv.St.Hooks(c.Channel.ID)
		if err != nil {
			return err
		}
		if len(hooks) == 0 {
			c.Notice("no hooks here yet — /hook new mints one")
			return nil
		}
		// Never the token: it is only shown once, at creation. Losing it means
		// minting a new one, which is a second of work and cannot leak history.
		for _, h := range hooks {
			c.Notice("%s · %s%s · added by %s on %s", h.ID, h.Kind, hookTarget(h), h.By, h.Created)
		}

	case "revoke":
		hid := c.Arg(1)
		if hid == "" {
			return errors.New("usage: /hook revoke <id>")
		}
		if err := c.Srv.St.DeleteHook(c.Channel.ID, hid); err != nil {
			return err
		}
		c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick,
			Body: "revoked hook " + hid}, "")

	default:
		return errors.New("usage: /hook new|in <name>|out <url>|list|revoke <id>")
	}
	return nil
}
