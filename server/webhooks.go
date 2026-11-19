package main

// Webhooks in and out (F11), on the same `hooks` table and the same rule as the
// git hook in devlog.go: the token is the whole credential, it is shown once,
// and revoking the row kills it.
//
//   - An incoming hook (`/hook in <name>`) is a URL that posts into ONE channel
//     under a name, as an agent line, so it carries the AGENT label in every
//     client. Its token is accepted at /api/hooks/in/ and nowhere else.
//   - An outgoing hook (`/hook out <url>`) POSTs every event in the channel to
//     a URL, signed with HMAC-SHA256 keyed by its token. The token never posts.
//
// E2E: in a +e channel the server holds ciphertext and no key, so an outgoing
// payload there carries the event's metadata (kind, actor, time, seq) and no
// body. Lines from an incoming hook are plaintext the server can read, in any
// channel: the sender is outside the channel and has no key to encrypt with.
// Both are in docs/protocol.md.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// hookIn bounds what one incoming hook can post: a CI run that fails in a loop
// is held to a line a second after a burst, the same shape as a person.
var hookIn = NewLimiter(1, 20)

var hookName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

func hookTarget(h Hook) string {
	switch h.Kind {
	case "in":
		return " as " + h.Name
	case "out":
		return " to " + h.URL
	}
	return ""
}

// ---- the command: /hook in <name>, /hook out <url> ----

func cmdWebhook(c *Ctx) error {
	if c.Channel.IsDM {
		// Both people in a DM are ops of it, so either could quietly send the
		// other's words to a URL. A webhook is a channel feature.
		return errors.New("webhooks are for channels, not DMs")
	}
	kind, arg := strings.ToLower(c.Arg(0)), strings.TrimSpace(c.Arg(1))
	var name, target string
	switch kind {
	case "in":
		if !hookName.MatchString(arg) {
			return errors.New("usage: /hook in <name>  (letters, digits, - and _, up to 32)")
		}
		// The name labels every line it posts, so it must not wear a member's
		// nick: the same rule a plugin's display name obeys.
		if _, err := c.Srv.St.UserByNick(arg); err == nil {
			return fmt.Errorf("%q is a member's nick; pick another name", arg)
		}
		name = arg
	case "out":
		if err := checkHookURL(arg); err != nil {
			return err
		}
		target = arg
	}
	h, err := c.Srv.St.CreateWebhook(c.Channel.ID, kind, name, target, c.User.Nick)
	if err != nil {
		return err
	}
	base := "https://" + c.Srv.Name + "/api/hooks/"
	if kind == "in" {
		c.Notice("hook %s: POST to this URL and the line appears here as %s", h.ID, name)
		c.Notice("%s", base+"in/"+h.Token)
		c.Notice(`body: {"text":"build 412 is green"} as JSON, or plain text`)
		if encryptedChannel(c.Channel) {
			c.Notice("#%s is +e: lines from this hook are not end-to-end encrypted", c.Channel.Name)
		}
	} else {
		c.Notice("hook %s: every event here is POSTed to %s", h.ID, target)
		c.Notice("signing secret: %s", h.Token)
		c.Notice("check X-Gliph-Signature: sha256=HMAC-SHA256(secret, raw body)")
		if encryptedChannel(c.Channel) {
			c.Notice("#%s is +e: deliveries carry who, when and what kind, never the text", c.Channel.Name)
		}
	}
	c.Notice("treat it as a password. revoke: /hook revoke %s", h.ID)
	// Everyone in the room learns that a hook exists. An outgoing one sends
	// their words somewhere, and that is not the op's secret to keep.
	what := "an incoming webhook " + name
	if kind == "out" {
		u, _ := url.Parse(target)
		what = "an outgoing webhook to " + u.Host
	}
	return c.Srv.Post(c.Channel, &Event{Kind: "notice", Actor: c.User.Nick, Body: "added " + what}, "")
}

func checkHookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(raw) > 500 {
		return errors.New("usage: /hook out <https://…>")
	}
	return nil
}

// ---- incoming ----

func (s *Server) handleInHook(w http.ResponseWriter, r *http.Request) {
	h, err := s.St.HookByToken(r.PathValue("token"))
	if err != nil || h.Kind != "in" {
		fail(w, 403, "unknown or revoked hook")
		return
	}
	if !hookIn.Allow(h.ID) {
		fail(w, 429, "too many lines; slow down")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, hookBodyMax))
	if err != nil {
		fail(w, 400, "unreadable body")
		return
	}
	text := string(body)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(body, &d) != nil {
			fail(w, 400, `expected {"text":"…"}`)
			return
		}
		text = d.Text
	}
	text = clean(text, 4000)
	if text == "" {
		fail(w, 400, "empty text")
		return
	}
	ch, err := s.St.ChannelByID(h.Channel)
	if err != nil {
		fail(w, 404, "the hook's channel is gone")
		return
	}
	if err := s.Post(ch, &Event{Kind: "agent", Actor: h.Name, Body: text,
		Meta: map[string]any{"webhook": h.ID}}, ""); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- outgoing ----

func (s *Store) OutHooks(cid string) ([]Hook, error) {
	rows, err := s.r.Query(`SELECT `+hookCols+` FROM hooks WHERE channel_id=? AND kind='out'`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hook
	for rows.Next() {
		var h Hook
		if err := scanHook(rows, &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// hookPayload is what an outgoing hook receives. Body is left out whenever the
// event is ciphertext: the server cannot read it, and base64 nobody can open
// is no use to a receiver either.
func hookPayload(h Hook, ch *Channel, e *Event) []byte {
	ev := map[string]any{"seq": e.Seq, "id": e.ID, "kind": e.Kind, "actor": e.Actor, "ts": e.TS, "enc": e.Enc}
	if !e.Enc {
		ev["body"] = e.Body
	}
	b, _ := json.Marshal(map[string]any{
		"hook": h.ID, "channel": map[string]any{"id": ch.ID, "name": ch.Name, "encrypted": encryptedChannel(ch)},
		"event": ev,
	})
	return b
}

func hookSignature(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// fireWebhooks runs off the hot path, from Post. A line that came in through
// a webhook is not sent back out: an in-hook and an out-hook pointed at each
// other would otherwise loop for as long as the rate limit allowed.
//
// ponytail: one query per posted event on the read pool, one attempt per
// delivery, no retries and no queue. Add a per-channel cache when the query
// shows up in profiles, and a retry queue when someone needs at-least-once.
func (s *Server) fireWebhooks(ch *Channel, e *Event) {
	if _, ok := e.Meta["webhook"]; ok {
		return
	}
	hooks, err := s.St.OutHooks(ch.ID)
	if err != nil || len(hooks) == 0 {
		return
	}
	for _, h := range hooks {
		body := hookPayload(h, ch, e)
		req, err := http.NewRequest("POST", h.URL, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "gliph-webhook")
		req.Header.Set("X-Gliph-Hook", h.ID)
		req.Header.Set("X-Gliph-Event", e.Kind)
		req.Header.Set("X-Gliph-Signature", hookSignature(body, h.Token))
		res, err := hookClient.Do(req)
		if err != nil {
			log.Printf("webhook %s: %v", h.ID, err)
			continue
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if res.StatusCode >= 300 {
			log.Printf("webhook %s: %s answered %d", h.ID, h.URL, res.StatusCode)
		}
	}
}

// hookAllowed is the SSRF line: an op can name any URL, so the server must not
// become their way into its own network (the cloud metadata address, the
// database, localhost). Checked on the address actually dialled, after DNS,
// so a name that resolves to 10.0.0.5 is refused as surely as the literal.
//
// ponytail: a self-host that wants a webhook on its own LAN cannot have one
// yet. Add a flag that relaxes this when someone asks.
var hookAllowed = func(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

var hookClient = &http.Client{
	Timeout: 5 * time.Second,
	// A redirect is a second URL nobody approved.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, _ := net.SplitHostPort(address)
				if ip := net.ParseIP(host); ip == nil || !hookAllowed(ip) {
					return fmt.Errorf("%s is not a public address", host)
				}
				return nil
			}}).DialContext,
	},
}
