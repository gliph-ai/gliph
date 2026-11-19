package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	st, err := OpenStore(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(st, "test")
	if _, err := st.CreateChannel("general", "hi", false); err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeConn captures frames a client would receive.
func fakeConn(s *Server, u *User) *Conn {
	c := &Conn{srv: s, user: u, out: make(chan Frame, 256)}
	s.addConn(c)
	return c
}

func drainCh(ch chan Frame) []Frame {
	var out []Frame
	for {
		select {
		case f := <-ch:
			out = append(out, f)
		default:
			return out
		}
	}
}

func drain(c *Conn) []Frame { return drainCh(c.out) }

func noticeText(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		if f.T == "notice" {
			m := f.D.(map[string]any)
			b.WriteString(m["text"].(string) + "\n")
		}
	}
	return b.String()
}

func TestBuiltinCommandsAndRoles(t *testing.T) {
	s := testServer(t)
	dana, err := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	if err != nil {
		t.Fatal(err)
	}
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")

	dc, rc := fakeConn(s, dana), fakeConn(s, robin)

	// a member cannot op anyone
	if err := s.RunCommand(rc, ch.ID, "op", "robin"); err == nil {
		t.Fatal("member was allowed to /op")
	}
	// an op can
	if err := s.RunCommand(dc, ch.ID, "op", "robin"); err != nil {
		t.Fatalf("op failed: %v", err)
	}
	if role, _ := s.St.MemberRole(ch.ID, robin.ID); role != "op" {
		t.Fatalf("robin role = %q, want op", role)
	}

	// /me posts an action event
	drain(dc)
	if err := s.RunCommand(dc, ch.ID, "me", "waves hello"); err != nil {
		t.Fatal(err)
	}
	events, _ := s.St.Since(ch.ID, 0, 100)
	last := events[len(events)-1]
	if last.Kind != "action" || last.Body != "waves hello" {
		t.Fatalf("got %+v, want action/waves hello", last)
	}

	// missing required argument is refused using the manifest's own arg spec
	if err := s.RunCommand(dc, ch.ID, "slap", ""); err == nil {
		t.Fatal("expected usage error for /slap with no target")
	}

	// unknown command
	if err := s.RunCommand(dc, ch.ID, "nope", ""); err == nil {
		t.Fatal("expected unknown command error")
	}

	// /away sets, /back clears (and so does a bare /away)
	for _, clear := range []string{"back", "away"} {
		s.RunCommand(dc, ch.ID, "away", "lunch")
		if u, _ := s.St.UserByID(dana.ID); u.Away != "lunch" {
			t.Fatalf("away = %q, want lunch", u.Away)
		}
		if err := s.RunCommand(dc, ch.ID, clear, ""); err != nil {
			t.Fatal(err)
		}
		if u, _ := s.St.UserByID(dana.ID); u.Away != "" {
			t.Fatalf("/%s left away = %q", clear, u.Away)
		}
	}
}

func TestModeratedChannelBlocksUnvoiced(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	mika, _ := s.St.CreateUser("mika", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, mika.ID, "member")
	dc, mc := fakeConn(s, dana), fakeConn(s, mika)

	if err := s.RunCommand(dc, ch.ID, "mode", "+m"); err != nil {
		t.Fatal(err)
	}
	if err := s.handleSend(mc, ch.ID, "can I speak?", "", false, 0, "", "", false); err == nil {
		t.Fatal("unvoiced user spoke in a +m channel")
	}
	if err := s.RunCommand(dc, ch.ID, "voice", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := s.handleSend(mc, ch.ID, "now I can", "", false, 0, "", "", false); err != nil {
		t.Fatalf("voiced user blocked: %v", err)
	}
}

func TestKickBanAndInviteOnly(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	troll, _ := s.St.CreateUser("troll", "t@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, troll.ID, "member")
	dc, tc := fakeConn(s, dana), fakeConn(s, troll)

	if err := s.RunCommand(dc, ch.ID, "kick", "troll being rude"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.St.MemberRole(ch.ID, troll.ID); ok {
		t.Fatal("troll still a member after kick")
	}
	events, _ := s.St.Since(ch.ID, 0, 100)
	last := events[len(events)-1]
	if last.Kind != "kick" || !strings.Contains(last.Body, "being rude") {
		t.Fatalf("kick reason not broadcast: %+v", last)
	}

	// ban keeps them out
	if err := s.RunCommand(dc, ch.ID, "ban", "troll spam"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(tc, "", "join", "general"); err == nil {
		t.Fatal("banned user rejoined")
	}

	// +i keeps out the uninvited, an invite lets them in
	s.RunCommand(dc, ch.ID, "mode", "+i")
	newbie, _ := s.St.CreateUser("newbie", "n@x.io", "hunter2hunter2", "member")
	nc := fakeConn(s, newbie)
	if err := s.RunCommand(nc, "", "join", "general"); err == nil {
		t.Fatal("uninvited user joined a +i channel")
	}
	if err := s.RunCommand(dc, ch.ID, "invite", "newbie"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(nc, "", "join", "general"); err != nil {
		t.Fatalf("invited user could not join: %v", err)
	}
}

// The headline behaviour: a plugin registers commands at runtime, every
// connected client is pushed a new palette, and calls route to the plugin.
func TestPluginCommandsAreRegisteredPushedAndRouted(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "admin")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	before := len(s.Reg.Manifest())
	if _, ok := s.Reg.Lookup("deploy"); ok {
		t.Fatal("/deploy exists before any plugin registered it")
	}

	meta, err := s.St.CreatePlugin("deploybot", "deploybot", "agent")
	if err != nil {
		t.Fatal(err)
	}
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 64)}
	s.mu.Lock()
	s.plugins[meta.ID] = p
	s.mu.Unlock()
	s.St.PluginJoin(meta.ID, ch.ID)

	drain(dc)
	reg := `{"t":"register","d":{"display":"deploybot","kind":"agent","commands":[
	  {"name":"deploy","usage":"/deploy <env>","desc":"ship it","min_role":"op","scope":"channel",
	   "args":[{"name":"env","type":"text","required":true}]}]}}`
	if err := s.handlePluginFrame(p, []byte(reg)); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// registry grew and the client was pushed a new palette containing /deploy
	if got := len(s.Reg.Manifest()); got != before+1 {
		t.Fatalf("manifest size %d, want %d", got, before+1)
	}
	var pushed bool
	for _, f := range drain(dc) {
		if f.T != "commands" {
			continue
		}
		b, _ := json.Marshal(f.D)
		if strings.Contains(string(b), `"name":"deploy"`) {
			pushed = true
		}
	}
	if !pushed {
		t.Fatal("client never received a commands manifest containing /deploy")
	}

	// a plugin may not shadow a builtin
	clash := `{"t":"register","d":{"commands":[{"name":"kick","desc":"nope"}]}}`
	if err := s.handlePluginFrame(p, []byte(clash)); err == nil {
		t.Fatal("plugin was allowed to shadow builtin /kick")
	}

	// invoking routes to the plugin with parsed named args
	if err := s.RunCommand(dc, ch.ID, "deploy", "staging"); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	var call string
	var sawInvoke bool
	for _, f := range drainCh(p.out) {
		if f.T == "invoke" {
			m := f.D.(map[string]any)
			call = m["call"].(string)
			if m["arg_map"].(map[string]string)["env"] != "staging" {
				t.Fatalf("arg_map = %+v, want env=staging", m["arg_map"])
			}
			sawInvoke = true
		}
	}
	if !sawInvoke {
		t.Fatal("plugin never received invoke")
	}

	// the plugin's result becomes an agent event in the channel
	res, _ := json.Marshal(map[string]any{"t": "result", "d": map[string]any{"call": call, "say": "shipping staging"}})
	if err := s.handlePluginFrame(p, res); err != nil {
		t.Fatal(err)
	}
	events, _ := s.St.Since(ch.ID, 0, 100)
	last := events[len(events)-1]
	if last.Kind != "agent" || last.Actor != "deploybot" || last.Body != "shipping staging" {
		t.Fatalf("got %+v, want agent/deploybot/shipping staging", last)
	}

	// disconnecting removes the commands and pushes the palette again
	drain(dc)
	s.Reg.UnregisterPlugin(meta.ID)
	if _, ok := s.Reg.Lookup("deploy"); ok {
		t.Fatal("/deploy survived plugin unregister")
	}
	if len(drain(dc)) == 0 {
		t.Fatal("no palette push after plugin unregister")
	}
}

func TestPluginCannotPostToChannelItIsNotIn(t *testing.T) {
	s := testServer(t)
	meta, _ := s.St.CreatePlugin("nosy", "nosy", "agent")
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 8)}
	priv, _ := s.St.CreateChannel("private", "", false)
	say, _ := json.Marshal(map[string]any{"t": "say", "d": map[string]any{"channel": priv.ID, "body": "hello"}})
	if err := s.handlePluginFrame(p, say); err == nil {
		t.Fatal("plugin posted into a channel it is not a member of")
	}
}

func TestEventLogOrderingAndNonceDedupe(t *testing.T) {
	s := testServer(t)
	u, _ := s.St.CreateUser("waseem", "w@x.io", "hunter2hunter2", "admin")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, u.ID, "op")

	for i := 0; i < 5; i++ {
		if err := s.Post(ch, &Event{Kind: "message", Actor: "waseem", Body: "m"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := s.St.Since(ch.ID, 0, 100)
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}

	// an offline client replaying its queue must not double-post
	if err := s.Post(ch, &Event{Kind: "message", Actor: "waseem", Body: "once"}, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(ch, &Event{Kind: "message", Actor: "waseem", Body: "once"}, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.St.Since(ch.ID, 0, 100)
	if len(after) != len(events)+1 {
		t.Fatalf("nonce replay duplicated: %d events, want %d", len(after), len(events)+1)
	}

	// cursor sync returns only what the client has not seen
	tail, _ := s.St.Since(ch.ID, 3, 100)
	if len(tail) != len(after)-3 {
		t.Fatalf("since(3) returned %d, want %d", len(tail), len(after)-3)
	}
}

func TestDMsAreTwoPersonAndReusable(t *testing.T) {
	s := testServer(t)
	a, _ := s.St.CreateUser("a", "a@x.io", "hunter2hunter2", "member")
	b, _ := s.St.CreateUser("b", "b@x.io", "hunter2hunter2", "member")
	ch1, err := s.DMChannel(a, b)
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := s.DMChannel(b, a) // reversed order must find the same room
	if err != nil {
		t.Fatal(err)
	}
	if ch1.ID != ch2.ID {
		t.Fatalf("DM channels differ: %s vs %s", ch1.ID, ch2.ID)
	}
	if !ch1.IsDM {
		t.Fatal("DM channel not marked is_dm")
	}
	// channel-scoped commands are refused inside a DM
	ac := fakeConn(s, a)
	if err := s.RunCommand(ac, ch1.ID, "invite", "b"); err == nil {
		t.Fatal("/invite was allowed inside a DM")
	}
}

// ---- rate limiting (spec F1: flood protection) ----

func TestLimiterBurstThenRefill(t *testing.T) {
	l := NewLimiter(1, 3) // 3 burst, one token per second
	t0 := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allowAt("u1", t0) {
			t.Fatalf("burst token %d denied", i)
		}
	}
	if l.allowAt("u1", t0) {
		t.Fatal("bucket handed out more than its burst")
	}
	// half a second buys nothing, a full second buys exactly one
	if l.allowAt("u1", t0.Add(500*time.Millisecond)) {
		t.Fatal("bucket refilled early")
	}
	if !l.allowAt("u1", t0.Add(time.Second)) {
		t.Fatal("bucket did not refill after a second")
	}
	if l.allowAt("u1", t0.Add(time.Second)) {
		t.Fatal("bucket refilled more than the rate allows")
	}
	// a different key has its own budget
	if !l.allowAt("u2", t0) {
		t.Fatal("one key's flood starved another")
	}
}

func TestLimiterPruneNeverGrantsBudget(t *testing.T) {
	// Pruning resets a bucket to full, so it is only safe once the bucket would
	// have refilled anyway. Every configured budget must satisfy that.
	for _, l := range NewLimits().all() {
		if l.rate*limiterIdle.Seconds() < l.burst {
			t.Fatalf("limiter rate=%v burst=%v refills slower than the %v prune window", l.rate, l.burst, limiterIdle)
		}
	}
	l := NewLimiter(1, 3)
	t0 := time.Now()
	l.allowAt("stale", t0)
	l.allowAt("fresh", t0.Add(time.Hour))
	l.pruneAt(t0.Add(time.Hour), limiterIdle)
	if n := l.len(); n != 1 {
		t.Fatalf("prune left %d buckets, want 1", n)
	}
}

// The headline acceptance case: 100 messages in a moment gets throttled, the
// user is told, and the channel does not flood.
func TestMessageBurstIsThrottledWithNotice(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "member")
	dc := fakeConn(s, dana)

	for i := 0; i < 100; i++ {
		raw, _ := json.Marshal(Frame{T: "message.send", D: map[string]any{"channel": ch.ID, "body": "flood"}})
		s.handleFrame(dc, raw)
	}

	events, _ := s.St.Since(ch.ID, 0, 200)
	if len(events) == 0 {
		t.Fatal("every message was dropped — a human would be throttled too")
	}
	if len(events) > 30 {
		t.Fatalf("%d of 100 flood messages reached the channel", len(events))
	}
	frames := drain(dc)
	notices := noticeText(frames)
	if !strings.Contains(notices, "too fast") {
		t.Fatalf("no visible throttle notice, got: %q", notices)
	}
	// the notice is an error-level notice carrying the channel, so a client can
	// render it in the right place
	var sawErr bool
	for _, f := range frames {
		if f.T != "notice" {
			continue
		}
		m := f.D.(map[string]any)
		if m["level"] == "error" && m["channel"] == ch.ID {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("throttle notice was not an error-level notice in the channel")
	}
	// dropping must never disconnect
	if !s.IsOnline(dana.ID) {
		t.Fatal("flooding client was disconnected instead of throttled")
	}
}

func TestRateLimitIsPerUserNotPerConnection(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "member")
	phone, laptop := fakeConn(s, dana), fakeConn(s, dana)

	// alternate devices: two connections must share one budget, not double it
	for i := 0; i < 100; i++ {
		c := phone
		if i%2 == 1 {
			c = laptop
		}
		raw, _ := json.Marshal(Frame{T: "message.send", D: map[string]any{"channel": ch.ID, "body": "flood"}})
		s.handleFrame(c, raw)
	}
	events, _ := s.St.Since(ch.ID, 0, 200)
	if len(events) > 30 {
		t.Fatalf("%d messages got through on two devices — the budget was per connection", len(events))
	}

	// and the per-user connection cap holds
	for i := 0; i < s.Limits.MaxConns+4; i++ {
		fakeConn(s, dana)
	}
	s.mu.RLock()
	n := len(s.conns[dana.ID])
	s.mu.RUnlock()
	if n > s.Limits.MaxConns {
		t.Fatalf("%d connections open for one user, cap is %d", n, s.Limits.MaxConns)
	}
}

func TestLoginRateLimitPerNickAndIP(t *testing.T) {
	s := testServer(t)
	s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	req := func(ip, nick string) bool {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = ip + ":51000"
		return s.loginAllowed(r, nick)
	}
	// guessing one account from one host runs out fast
	var ok int
	for i := 0; i < 20; i++ {
		if req("203.0.113.5", "dana") {
			ok++
		}
	}
	if ok > 5 {
		t.Fatalf("%d password guesses allowed against one nick, want <= 5", ok)
	}
	// a different host guessing the same nick is still blocked (per-nick budget)
	if req("198.51.100.9", "dana") {
		t.Fatal("distributed guessing bypassed the per-nick limit")
	}
	// an untouched account from a fresh host is unaffected
	if !req("198.51.100.9", "robin") {
		t.Fatal("an unrelated login was collateral damage")
	}
	// XFF is honoured only from a trusted proxy (loopback by default; Pushleft's
	// network when configured), so one proxy does not become one bucket
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:51000"
	r.Header.Set("X-Forwarded-For", "198.51.100.77")
	if got := clientIP(r); got != "198.51.100.77" {
		t.Fatalf("clientIP behind loopback proxy = %q", got)
	}
	r.RemoteAddr = "[::ffff:10.128.4.1]:51000"
	if got := clientIP(r); got != "::ffff:10.128.4.1" {
		t.Fatalf("an unconfigured private peer's XFF was believed: %q", got)
	}
	defer func(old []*net.IPNet) { trustedProxies = old }(trustedProxies)
	trustedProxies = mustCIDRs("10.128.0.0/9")
	if got := clientIP(r); got != "198.51.100.77" {
		t.Fatalf("clientIP behind Pushleft's proxy = %q", got)
	}
	r.RemoteAddr = "203.0.113.5:51000"
	if got := clientIP(r); got != "203.0.113.5" {
		t.Fatalf("clientIP trusted a remote peer's XFF header: %q", got)
	}
}

// ---- metrics ----

func TestMetricsParseAndLeakNothing(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	fakeConn(s, dana)
	if err := s.Post(ch, &Event{Kind: "message", Actor: "dana", Body: "secret plans for the weekend"}, ""); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	s.writeMetrics(&buf)
	out := buf.String()

	// every non-comment line must be `name value` with a numeric value
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, val, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("unparseable exposition line: %q", line)
		}
		if strings.ContainsAny(name, "{}") {
			t.Fatalf("metric %q carries labels — labels are how identifiers leak", name)
		}
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			t.Fatalf("metric %s has non-numeric value %q", name, val)
		}
		seen[name] = val
	}

	for _, want := range []string{"gliph_connections", "gliph_users_total", "gliph_channels_total",
		"gliph_events_total", "gliph_messages_dropped_ratelimit_total", "gliph_plugins_connected",
		"gliph_uploads_bytes", "gliph_db_bytes", "go_goroutines", "go_memstats_alloc_bytes"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing metric %s", want)
		}
	}
	if seen["gliph_connections"] != "1" {
		t.Fatalf("gliph_connections = %s, want 1", seen["gliph_connections"])
	}
	if seen["gliph_db_bytes"] == "0" {
		t.Fatal("gliph_db_bytes is 0 — the page_count/page_size query did not run")
	}

	// PRIVACY: no nick, no channel name, no channel/user id, no message body.
	for _, leak := range []string{"dana", "general", ch.ID, dana.ID, "secret plans", "weekend"} {
		if strings.Contains(out, leak) {
			t.Fatalf("/metrics leaked %q", leak)
		}
	}
}

// ---- end-to-end encryption ----
//
// The server-side guarantee is narrow and worth pinning down precisely: in a
// +e channel it stores ciphertext it cannot interpret, it hands a sealed key
// only to the device it was addressed to, and it refuses to let a client
// smuggle a key to a device outside the channel.

func TestEncryptedChannelStoresNoPlaintext(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	if err := s.RunCommand(dc, ch.ID, "mode", "+e"); err != nil {
		t.Fatal(err)
	}
	ch, _ = s.St.ChannelByID(ch.ID)
	if !encryptedChannel(ch) {
		t.Fatal("+e did not stick")
	}

	// plaintext is refused outright in a +e channel
	if err := s.handleSend(dc, ch.ID, "in the clear", "", false, 0, "", "", false); err == nil {
		t.Fatal("server accepted plaintext into a +e channel")
	}
	// and ciphertext needs an epoch
	if err := s.handleSend(dc, ch.ID, "AAAA", "", true, 0, "", "", false); err == nil {
		t.Fatal("server accepted ciphertext with no key epoch")
	}

	secret := "the freeze ships friday"
	ct := base64.StdEncoding.EncodeToString([]byte("not-really-encrypted:" + secret))
	if err := s.handleSend(dc, ch.ID, ct, "", true, 1, "", "", false); err != nil {
		t.Fatalf("encrypted send failed: %v", err)
	}

	// what landed in the log is the ciphertext, flagged, with its epoch
	events, _ := s.St.Since(ch.ID, 0, 100)
	last := events[len(events)-1]
	if !last.Enc || last.Epoch != 1 || last.Body != ct {
		t.Fatalf("stored event wrong: enc=%v epoch=%d body=%q", last.Enc, last.Epoch, last.Body)
	}

	// and the plaintext appears nowhere in the database file
	var dump strings.Builder
	rows, err := s.St.db.Query(`SELECT id,channel_id,kind,actor,body,meta FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c2, d2, e2, f2 string
		if err := rows.Scan(&a, &b, &c2, &d2, &e2, &f2); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(a + b + c2 + d2 + e2 + f2)
	}
	if strings.Contains(dump.String(), secret) {
		t.Fatal("plaintext of an encrypted message is readable in the events table")
	}
}

func TestSealedKeysGoOnlyToTheAddressedDevice(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	outsider, _ := s.St.CreateUser("mallory", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")

	if _, err := s.St.UpsertDevice(dana.ID, "dev-dana", "mac", "PUBKEY-DANA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.UpsertDevice(robin.ID, "dev-robin", "phone", "PUBKEY-ROBIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.UpsertDevice(outsider.ID, "dev-mallory", "pc", "PUBKEY-MALLORY"); err != nil {
		t.Fatal(err)
	}
	// a device id belongs to one account for good
	if _, err := s.St.UpsertDevice(robin.ID, "dev-dana", "stolen", "PUBKEY-ROBIN"); err == nil {
		t.Fatal("a second account was allowed to claim an existing device id")
	}
	markSigned(t, s, "dev-dana", "dev-robin", "dev-mallory")

	dc := fakeConn(s, dana)
	dc.device = "dev-dana"

	// publishing includes an entry for a device outside the channel; the server
	// must drop it rather than store a key addressed to an outsider
	pub, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "epoch": 1,
		"entries": map[string]string{
			"dev-dana": "SEALED-FOR-DANA", "dev-robin": "SEALED-FOR-ROBIN",
			"dev-mallory": "SEALED-FOR-MALLORY",
		},
	})
	if !s.handleCryptoFrame(dc, "keys.publish", pub) {
		t.Fatal("keys.publish was not handled")
	}
	if got, _ := s.St.ChannelKeysFor(ch.ID, "dev-mallory"); len(got) != 0 {
		t.Fatalf("a key was sealed to a device outside the channel: %+v", got)
	}

	// each member device gets exactly its own blob
	danaKeys, _ := s.St.ChannelKeysFor(ch.ID, "dev-dana")
	if len(danaKeys) != 1 || danaKeys[0]["wrapped"] != "SEALED-FOR-DANA" {
		t.Fatalf("dana got %+v", danaKeys)
	}
	robinKeys, _ := s.St.ChannelKeysFor(ch.ID, "dev-robin")
	if len(robinKeys) != 1 || robinKeys[0]["wrapped"] != "SEALED-FOR-ROBIN" {
		t.Fatalf("robin got %+v", robinKeys)
	}

	// fetching is scoped to the calling connection's own device
	rc := fakeConn(s, robin)
	rc.device = "dev-dana" // claim someone else's device
	drain(rc)
	fetch, _ := json.Marshal(map[string]any{"channel": ch.ID})
	s.handleCryptoFrame(rc, "keys.fetch", fetch)
	if !strings.Contains(noticeText(drain(rc)), "unknown device") {
		t.Fatal("a user was able to fetch keys for a device that is not theirs")
	}

	// a non-member cannot even ask who the peers are
	oc := fakeConn(s, outsider)
	oc.device = "dev-mallory"
	drain(oc)
	peers, _ := json.Marshal(map[string]any{"channel": ch.ID})
	s.handleCryptoFrame(oc, "keys.peers", peers)
	if !strings.Contains(noticeText(drain(oc)), "not in that channel") {
		t.Fatal("a non-member could enumerate channel devices")
	}
}

func TestRekeyingListsOnlyDevicesMissingTheEpoch(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")
	s.St.UpsertDevice(dana.ID, "dev-dana", "mac", "PK1")
	s.St.UpsertDevice(robin.ID, "dev-robin", "phone", "PK2")
	markSigned(t, s, "dev-dana", "dev-robin")

	s.St.PutChannelKeys(ch.ID, 1, "dev-dana", map[string]string{"dev-dana": "X"})
	missing, err := s.St.DevicesMissingEpoch(ch.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].ID != "dev-robin" {
		t.Fatalf("missing = %+v, want just dev-robin", missing)
	}
	if s.St.MaxEpoch(ch.ID) != 1 {
		t.Fatalf("max epoch = %d, want 1", s.St.MaxEpoch(ch.ID))
	}

	// a reinstalled device (new public key) loses its old sealed keys, so it
	// gets re-keyed instead of silently failing to decrypt forever
	s.St.PutChannelKeys(ch.ID, 1, "dev-dana", map[string]string{"dev-robin": "Y"})
	if got, _ := s.St.ChannelKeysFor(ch.ID, "dev-robin"); len(got) != 1 {
		t.Fatal("robin should hold epoch 1")
	}
	s.St.UpsertDevice(robin.ID, "dev-robin", "phone", "PK2-NEW")
	if got, _ := s.St.ChannelKeysFor(ch.ID, "dev-robin"); len(got) != 0 {
		t.Fatal("stale sealed keys survived a device key change")
	}
}

// ---- blobs ----
//
// The framework's job is to enforce what providers only declare. These pin the
// three rules that matter: membership, role, and that an event can only name an
// action the document actually rendered.

func TestBlobTodosRoundTrip(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	open, _ := json.Marshal(map[string]any{"channel": ch.ID, "blob": "todos"})
	if !s.handleBlobFrame(dc, "blob.open", open) {
		t.Fatal("blob.open was not handled")
	}
	drain(dc)

	add, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "blob": "todos", "action": "add", "value": "ship the freeze"})
	s.handleBlobFrame(dc, "blob.event", add)

	todos, _ := s.St.Todos(ch.ID)
	if len(todos) != 1 || todos[0].Body != "ship the freeze" {
		t.Fatalf("todos = %+v", todos)
	}

	// toggling it done goes through the item's own action
	tog, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "blob": "todos", "action": "toggle", "item": todos[0].ID})
	s.handleBlobFrame(dc, "blob.event", tog)
	todos, _ = s.St.Todos(ch.ID)
	if !todos[0].Done {
		t.Fatal("todo did not toggle done")
	}

	// the shared document is stored, so a second viewer sees the same thing
	if _, err := s.St.BlobDoc(ch.ID, "todos"); err != nil {
		t.Fatalf("shared doc was not published: %v", err)
	}
}

func TestBlobRefusesActionsTheDocumentNeverOffered(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	// no document sent yet: every action is unavailable
	ev, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "blob": "todos", "action": "delete", "item": "t_whatever"})
	s.handleBlobFrame(dc, "blob.event", ev)
	if !strings.Contains(noticeText(drain(dc)), "not available") {
		t.Fatal("an action was accepted before any document offered it")
	}

	// after opening, a made-up action is still refused
	open, _ := json.Marshal(map[string]any{"channel": ch.ID, "blob": "todos"})
	s.handleBlobFrame(dc, "blob.open", open)
	drain(dc)
	bogus, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "blob": "todos", "action": "rm -rf", "item": "x"})
	s.handleBlobFrame(dc, "blob.event", bogus)
	if !strings.Contains(noticeText(drain(dc)), "not available") {
		t.Fatal("an unrendered action id was accepted")
	}
}

func TestBlobEnforcesRoleAndMembership(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	mika, _ := s.St.CreateUser("mika", "m@x.io", "hunter2hunter2", "member")
	outsider, _ := s.St.CreateUser("mallory", "x@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, mika.ID, "member")

	// a non-member cannot even open a blob in that channel
	oc := fakeConn(s, outsider)
	open, _ := json.Marshal(map[string]any{"channel": ch.ID, "blob": "checkins"})
	s.handleBlobFrame(oc, "blob.open", open)
	if !strings.Contains(noticeText(drain(oc)), "not in that channel") {
		t.Fatal("a non-member opened a blob")
	}

	// "remind" is min_role op: a plain member is refused, an op is not
	mc := fakeConn(s, mika)
	s.handleBlobFrame(mc, "blob.open", open)
	drain(mc)
	remind, _ := json.Marshal(map[string]any{
		"channel": ch.ID, "blob": "checkins", "action": "remind"})
	s.handleBlobFrame(mc, "blob.event", remind)
	if !strings.Contains(noticeText(drain(mc)), "needs op") {
		t.Fatal("a member was allowed to run an op-only blob action")
	}

	dc := fakeConn(s, dana)
	s.handleBlobFrame(dc, "blob.open", open)
	drain(dc)
	s.handleBlobFrame(dc, "blob.event", remind)
	if txt := noticeText(drain(dc)); strings.Contains(txt, "needs op") {
		t.Fatalf("an op was refused their own action: %s", txt)
	}
}

func TestBlobSwitcherListsBuiltinsPerChannel(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	got := map[string]bool{}
	for _, b := range s.blobsFor(ch) {
		got[b["id"].(string)] = true
	}
	for _, want := range []string{"todos", "pins", "checkins"} {
		if !got[want] {
			t.Fatalf("builtin blob %q missing from the switcher: %+v", want, got)
		}
	}
}

func TestRetroAliasesAndHighlightsRoundTrip(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "member")
	dc := fakeConn(s, dana)

	if err := s.RunCommand(dc, ch.ID, "alias", "gm hello everyone"); err != nil {
		t.Fatalf("/alias: %v", err)
	}
	if got := s.St.AliasesFor(dana.ID)["gm"]; got != "hello everyone" {
		t.Fatalf("alias gm = %q, want %q", got, "hello everyone")
	}
	// an alias must never shadow a real command
	if err := s.RunCommand(dc, ch.ID, "alias", "kick nope"); err == nil {
		t.Fatal("/alias was allowed to shadow /kick")
	}
	// and it reaches the client through the same map the state frame carries
	prefs := s.UserPrefs(dana.ID)
	if prefs["aliases"].(map[string]string)["gm"] != "hello everyone" {
		t.Fatalf("UserPrefs missing the alias: %+v", prefs)
	}

	drain(dc)
	if err := s.RunCommand(dc, ch.ID, "aliases", ""); err != nil {
		t.Fatalf("/aliases: %v", err)
	}
	if !strings.Contains(noticeText(drain(dc)), "/gm") {
		t.Fatal("/aliases did not list the alias")
	}
	if err := s.RunCommand(dc, ch.ID, "unalias", "gm"); err != nil {
		t.Fatalf("/unalias: %v", err)
	}
	if len(s.St.AliasesFor(dana.ID)) != 0 {
		t.Fatal("alias survived /unalias")
	}

	// highlights fold case, so a word pings you however it was typed
	if err := s.RunCommand(dc, ch.ID, "highlight", "Ship It"); err != nil {
		t.Fatalf("/highlight: %v", err)
	}
	if hl := s.St.HighlightsFor(dana.ID); len(hl) != 1 || hl[0] != "ship it" {
		t.Fatalf("highlights = %v, want [ship it]", hl)
	}
	if err := s.RunCommand(dc, ch.ID, "highlight", "bad\x07word"); err == nil {
		t.Fatal("a control character was accepted as a highlight word")
	}
	if err := s.RunCommand(dc, ch.ID, "unhighlight", "SHIP IT"); err != nil {
		t.Fatalf("/unhighlight: %v", err)
	}
	if len(s.St.HighlightsFor(dana.ID)) != 0 {
		t.Fatal("highlight survived /unhighlight")
	}
}

func TestRetroNotifyListIsVisibleAndFires(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	drain(dc)
	drain(rc)

	if err := s.RunCommand(dc, "", "notify", "robin"); err != nil {
		t.Fatalf("/notify: %v", err)
	}
	// opt-in, and visible to the person being watched
	if !strings.Contains(noticeText(drain(rc)), "notify list") {
		t.Fatal("the watched user was not told")
	}
	if s.St.WatcherCount(robin.ID) != 1 {
		t.Fatalf("watcher count = %d, want 1", s.St.WatcherCount(robin.ID))
	}

	drain(dc)
	if err := s.RunCommand(dc, "", "notify", ""); err != nil {
		t.Fatalf("/notify list: %v", err)
	}
	if !strings.Contains(noticeText(drain(dc)), "robin") {
		t.Fatal("/notify did not list robin")
	}

	drain(dc)
	s.NotifyOnline(robin.ID)
	if !strings.Contains(noticeText(drain(dc)), "robin is online") {
		t.Fatal("the watcher was not told robin connected")
	}

	if err := s.RunCommand(dc, "", "unnotify", "robin"); err != nil {
		t.Fatalf("/unnotify: %v", err)
	}
	drain(dc)
	s.NotifyOnline(robin.ID)
	if strings.Contains(noticeText(drain(dc)), "is online") {
		t.Fatal("removal did not stop the alerts")
	}
	if s.St.WatcherCount(robin.ID) != 0 {
		t.Fatal("watcher count did not drop")
	}
}

func TestRetroSoundNeedsPlusSAndIsRateLimited(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	if err := s.RunCommand(dc, ch.ID, "sound", "tada"); err == nil {
		t.Fatal("/sound worked in a channel without +s")
	}
	s.St.SetModes(ch.ID, "nts")

	if err := s.RunCommand(dc, ch.ID, "sound", "tada"); err != nil {
		t.Fatalf("/sound in a +s channel: %v", err)
	}
	if err := s.RunCommand(dc, ch.ID, "sound", "tada"); err == nil {
		t.Fatal("a second /sound within 30s was not rate limited")
	}

	events, _ := s.St.Since(ch.ID, 0, 50)
	sounds := 0
	for _, e := range events {
		if e.Kind == "sound" {
			sounds++
			if e.Meta["sound"] != "tada" {
				t.Fatalf("sound meta = %+v", e.Meta)
			}
		}
	}
	if sounds != 1 {
		t.Fatalf("%d sound events posted, want 1", sounds)
	}
}

func TestRetroCTCPPrivacyToggle(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	drain(dc)

	if err := s.RunCommand(dc, "", "ctcp", "robin version"); err != nil {
		t.Fatalf("/ctcp version: %v", err)
	}
	if !strings.Contains(noticeText(drain(dc)), "no devices registered") {
		t.Fatal("version did not report an empty device list")
	}
	s.St.UpsertDevice(robin.ID, "dev-robin", "phone", "PK1")
	s.RunCommand(dc, "", "ctcp", "robin version")
	if !strings.Contains(noticeText(drain(dc)), "phone") {
		t.Fatal("version did not name robin's device")
	}

	// robin declines, and the server stops answering on their behalf
	if err := s.RunCommand(rc, "", "ctcp", "off"); err != nil {
		t.Fatalf("/ctcp off: %v", err)
	}
	if err := s.RunCommand(dc, "", "ctcp", "robin time"); err == nil {
		t.Fatal("CTCP was answered for a user who opted out")
	}

	if err := s.RunCommand(rc, "", "ctcp", "on"); err != nil {
		t.Fatalf("/ctcp on: %v", err)
	}
	drain(dc)
	if err := s.RunCommand(dc, "", "ctcp", "robin time"); err != nil {
		t.Fatalf("/ctcp time: %v", err)
	}
	if !strings.Contains(noticeText(drain(dc)), "CTCP TIME") {
		t.Fatal("time query was not answered")
	}
}

// ---- P2P signalling (spec F7: the server relays, and relays nothing else) ----

// sendFrame pushes a frame in through the real dispatcher, so these tests cover
// the p2p.* routing in handleFrame as well as the handler itself.
func sendFrame(t *testing.T, s *Server, c *Conn, typ string, d map[string]any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"t": typ, "d": d})
	if err != nil {
		t.Fatal(err)
	}
	s.handleFrame(c, raw)
}

func framesOfType(frames []Frame, typ string) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f.T == typ {
			out = append(out, f.D.(map[string]any))
		}
	}
	return out
}

func TestP2PRelaysSignallingBetweenDMMembers(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, err := s.DMChannel(dana, robin)
	if err != nil {
		t.Fatal(err)
	}
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	drain(dc)
	drain(rc)

	digest := strings.Repeat("ab", 32)
	sendFrame(t, s, dc, "p2p.begin", map[string]any{
		"channel": ch.ID, "id": "tr1", "name": "holiday.zip", "size": 4096, "sha256": digest})
	sendFrame(t, s, dc, "p2p.offer", map[string]any{
		"channel": ch.ID, "id": "tr1", "sdp": "v=0 opaque to the server"})

	got := drain(rc)
	begins := framesOfType(got, "p2p.begin")
	if len(begins) != 1 {
		t.Fatalf("robin got %d p2p.begin frames, want 1", len(begins))
	}
	if begins[0]["name"] != "holiday.zip" || begins[0]["sha256"] != digest || begins[0]["from"] != "dana" {
		t.Fatalf("begin relayed wrong: %+v", begins[0])
	}
	offers := framesOfType(got, "p2p.offer")
	if len(offers) != 1 || offers[0]["sdp"] != "v=0 opaque to the server" {
		t.Fatalf("offer relayed wrong: %+v", offers)
	}
	// The sender gets no echo and no error.
	if back := drain(dc); len(back) != 0 {
		t.Fatalf("sender received frames back: %+v", back)
	}

	// The answer travels the other way, and the accept/reject pair too.
	sendFrame(t, s, rc, "p2p.accept", map[string]any{"channel": ch.ID, "id": "tr1"})
	sendFrame(t, s, rc, "p2p.answer", map[string]any{"channel": ch.ID, "id": "tr1", "sdp": "v=0 answer"})
	back := drain(dc)
	if len(framesOfType(back, "p2p.accept")) != 1 {
		t.Fatalf("accept was not relayed back: %+v", back)
	}
	if a := framesOfType(back, "p2p.answer"); len(a) != 1 || a[0]["sdp"] != "v=0 answer" {
		t.Fatalf("answer was not relayed back: %+v", back)
	}
}

func TestP2PRefusesNonDMChannel(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	drain(dc)
	drain(rc)

	sendFrame(t, s, dc, "p2p.offer", map[string]any{
		"channel": ch.ID, "id": "tr1", "sdp": "v=0 offer"})

	if txt := noticeText(drain(dc)); !strings.Contains(txt, "direct message") {
		t.Fatalf("expected a DM-only notice, got %q", txt)
	}
	if got := drain(rc); len(got) != 0 {
		t.Fatalf("offer leaked into a non-DM channel: %+v", got)
	}
}

func TestP2PRefusesSenderWhoIsNotAMember(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	mika, _ := s.St.CreateUser("mika", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.DMChannel(dana, robin)
	dcn, rc, mc := fakeConn(s, dana), fakeConn(s, robin), fakeConn(s, mika)
	drain(dcn)
	drain(rc)
	drain(mc)

	// mika is a stranger to this DM: naming its channel id must not relay.
	sendFrame(t, s, mc, "p2p.offer", map[string]any{
		"channel": ch.ID, "id": "tr1", "sdp": "v=0 offer"})

	if txt := noticeText(drain(mc)); !strings.Contains(txt, "not in that channel") {
		t.Fatalf("expected a membership notice, got %q", txt)
	}
	if got := append(drain(dcn), drain(rc)...); len(got) != 0 {
		t.Fatalf("outsider's offer reached the DM members: %+v", got)
	}
}

func TestP2PDropsMalformedFramesWithANotice(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.DMChannel(dana, robin)
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	drain(dc)
	drain(rc)

	bad := []map[string]any{
		{"channel": ch.ID, "sdp": "v=0"},                                    // no transfer id
		{"channel": ch.ID, "id": "tr1"},                                     // no sdp
		{"channel": ch.ID, "id": "tr1", "sdp": strings.Repeat("x", 32<<10)}, // oversized sdp
		{"channel": "c_nope", "id": "tr1", "sdp": "v=0"},                    // unknown channel
	}
	for _, d := range bad {
		sendFrame(t, s, dc, "p2p.offer", d)
	}
	// A begin with a filename that is not renderable, and one with no digest.
	sendFrame(t, s, dc, "p2p.begin", map[string]any{
		"channel": ch.ID, "id": "tr2", "name": "ev\nil.zip", "size": 10, "sha256": strings.Repeat("ab", 32)})
	sendFrame(t, s, dc, "p2p.begin", map[string]any{
		"channel": ch.ID, "id": "tr2", "name": "ok.zip", "size": 10, "sha256": "nothex"})

	notices := framesOfType(drain(dc), "notice")
	if len(notices) != len(bad)+2 {
		t.Fatalf("got %d notices, want %d: %+v", len(notices), len(bad)+2, notices)
	}
	if got := drain(rc); len(got) != 0 {
		t.Fatalf("a malformed frame was relayed: %+v", got)
	}
}

// ---- devlog ----

// devlogSetup is an op in #general plus one live git hook — the starting point
// for every devlog test.
func devlogSetup(t *testing.T) (*Server, *Channel, *Conn, string) {
	t.Helper()
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)
	h, err := s.St.CreateHook(ch.ID, "git", "dana")
	if err != nil {
		t.Fatal(err)
	}
	return s, ch, dc, h.Token
}

func deliverHook(s *Server, tok string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.DevlogRoutes(mux)
	r := httptest.NewRequest("POST", "/api/hooks/git/"+tok, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func githubSig(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

var ghPush = []byte(`{"ref":"refs/heads/main",
  "repository":{"full_name":"gliph/gliph","name":"gliph"},
  "pusher":{"name":"waseem"},
  "commits":[{"id":"abc123","message":"fix cursor replay\n\ndetails","url":"https://gh/c/abc123"},
             {"id":"def456","message":"tidy the blob router","url":"https://gh/c/def456"}]}`)

func devlogEvents(t *testing.T, s *Server, cid string) []*Event {
	t.Helper()
	evs, err := s.St.DevlogEvents(cid, 200)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestGitHubPushWithGoodSignatureLandsOneEvent(t *testing.T) {
	s, ch, _, tok := devlogSetup(t)
	w := deliverHook(s, tok, ghPush, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(ghPush, tok),
	})
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	evs := devlogEvents(t, s, ch.ID)
	if len(evs) != 1 {
		t.Fatalf("want one event per push, got %d", len(evs))
	}
	e := evs[0]
	if e.Kind != "commit" || e.Actor != "waseem" {
		t.Fatalf("got %+v", e)
	}
	want := "push to gliph/main by waseem — 2 commits: fix cursor replay; tidy the blob router"
	if e.Body != want {
		t.Fatalf("body = %q, want %q", e.Body, want)
	}
	if e.Meta["repo"] != "gliph/gliph" || e.Meta["branch"] != "main" || e.Meta["pusher"] != "waseem" {
		t.Fatalf("meta = %+v", e.Meta)
	}
	commits, _ := e.Meta["commits"].([]any)
	if len(commits) != 2 {
		t.Fatalf("meta commits = %+v", e.Meta["commits"])
	}
	first, _ := commits[0].(map[string]any)
	if first["id"] != "abc123" || first["msg"] != "fix cursor replay" || first["url"] != "https://gh/c/abc123" {
		t.Fatalf("commit meta = %+v", first)
	}

	// the blob renders that push, with the head commit's URL on the row
	doc, err := devlogDoc(s, ch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.actionIDs()["open:https://gh/c/def456"]; !ok {
		t.Fatalf("no open action for the head commit: %+v", doc.actionIDs())
	}
}

func TestGitHubPushWithBadSignaturePostsNothing(t *testing.T) {
	s, ch, _, tok := devlogSetup(t)
	for _, sig := range []string{"", "sha256=deadbeef", githubSig(ghPush, "not-the-secret")} {
		w := deliverHook(s, tok, ghPush, map[string]string{
			"X-GitHub-Event": "push", "X-Hub-Signature-256": sig})
		if w.Code != 403 {
			t.Fatalf("signature %q gave %d, want 403", sig, w.Code)
		}
	}
	// a body that does not match its own (otherwise valid) signature
	w := deliverHook(s, tok, []byte(`{"ref":"refs/heads/evil","commits":[]}`),
		map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(ghPush, tok)})
	if w.Code != 403 {
		t.Fatalf("tampered body gave %d, want 403", w.Code)
	}
	if evs := devlogEvents(t, s, ch.ID); len(evs) != 0 {
		t.Fatalf("a forged push reached the channel: %+v", evs)
	}
}

func TestGitLabPushChecksItsToken(t *testing.T) {
	s, ch, _, tok := devlogSetup(t)
	body := []byte(`{"ref":"refs/heads/dev",
	  "project":{"path_with_namespace":"team/gliph"},"user_username":"robin",
	  "commits":[{"id":"f00","message":"gitlab commit","url":"https://gl/c/f00"}]}`)

	if w := deliverHook(s, tok, body, map[string]string{
		"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong"}); w.Code != 403 {
		t.Fatalf("wrong gitlab token gave %d, want 403", w.Code)
	}
	if evs := devlogEvents(t, s, ch.ID); len(evs) != 0 {
		t.Fatal("an unverified gitlab push reached the channel")
	}

	if w := deliverHook(s, tok, body, map[string]string{
		"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": tok}); w.Code != 200 {
		t.Fatalf("good gitlab token gave %d", w.Code)
	}
	evs := devlogEvents(t, s, ch.ID)
	if len(evs) != 1 || evs[0].Body != "push to gliph/dev by robin — 1 commit: gitlab commit" {
		t.Fatalf("gitlab push = %+v", evs)
	}
	if evs[0].Meta["repo"] != "team/gliph" {
		t.Fatalf("gitlab meta = %+v", evs[0].Meta)
	}
}

func TestGitHookIgnoresNonPushAndEmptyPushes(t *testing.T) {
	s, ch, _, tok := devlogSetup(t)
	ping := []byte(`{"zen":"keep it logically awesome"}`)
	w := deliverHook(s, tok, ping, map[string]string{
		"X-GitHub-Event": "ping", "X-Hub-Signature-256": githubSig(ping, tok)})
	if w.Code != 200 {
		t.Fatalf("ping gave %d, want 200 so the provider stops retrying", w.Code)
	}
	if len(devlogEvents(t, s, ch.ID)) != 0 {
		t.Fatal("a ping was posted to the channel")
	}

	// branch delete: zero commits, still one readable event
	del := []byte(`{"ref":"refs/heads/old","repository":{"full_name":"gliph/gliph"},
	  "pusher":{"name":"waseem"},"deleted":true,"commits":[]}`)
	w = deliverHook(s, tok, del, map[string]string{
		"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(del, tok)})
	if w.Code != 200 {
		t.Fatalf("zero-commit push gave %d", w.Code)
	}
	evs := devlogEvents(t, s, ch.ID)
	if len(evs) != 1 || !strings.Contains(evs[0].Body, "no commits") {
		t.Fatalf("zero-commit push = %+v", evs)
	}
	if _, err := devlogDoc(s, ch.ID, ""); err != nil {
		t.Fatalf("the blob choked on a commit-less push: %v", err)
	}
}

func TestDevlogLogCommandAndFilter(t *testing.T) {
	s, ch, dc, tok := devlogSetup(t)
	if err := s.RunCommand(dc, ch.ID, "log", "ran ANALYZE on prod db — 40s, lock-free"); err != nil {
		t.Fatalf("/log: %v", err)
	}
	evs := devlogEvents(t, s, ch.ID)
	if len(evs) != 1 || evs[0].Kind != "log" || evs[0].Actor != "dana" {
		t.Fatalf("/log posted %+v", evs)
	}

	deliverHook(s, tok, ghPush, map[string]string{
		"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(ghPush, tok)})

	doc, err := devlogDoc(s, ch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(doc.Blocks[1].Items); n != 2 {
		t.Fatalf("unfiltered devlog has %d rows, want 2", n)
	}
	// the filter matches text, and repo/branch/author through the hint
	for _, tc := range []struct {
		filter string
		want   string
	}{{"ANALYZE", "log"}, {"gliph", "commit"}, {"waseem", "commit"}} {
		doc, _ := devlogDoc(s, ch.ID, tc.filter)
		items := doc.Blocks[1].Items
		if len(items) != 1 {
			t.Fatalf("filter %q matched %d rows", tc.filter, len(items))
		}
	}
	if doc, _ := devlogDoc(s, ch.ID, "nothing-like-this"); doc.Blocks[1].T != "text" {
		t.Fatal("an empty filter result still rendered a list")
	}
}

func TestHookRevokeKillsAPreviouslyGoodToken(t *testing.T) {
	s, ch, dc, _ := devlogSetup(t)
	if err := s.RunCommand(dc, ch.ID, "hook", "new"); err != nil {
		t.Fatalf("/hook new: %v", err)
	}
	txt := noticeText(drain(dc))
	if !strings.Contains(txt, "/api/hooks/git/") {
		t.Fatalf("/hook new printed no webhook URL: %s", txt)
	}
	hooks, _ := s.St.Hooks(ch.ID)
	var h Hook
	for _, x := range hooks {
		if strings.Contains(txt, x.Token) {
			h = x
		}
	}
	if h.ID == "" {
		t.Fatalf("the minted token was never printed: %s", txt)
	}

	// it works before the revoke
	w := deliverHook(s, h.Token, ghPush, map[string]string{
		"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(ghPush, h.Token)})
	if w.Code != 200 {
		t.Fatalf("fresh hook gave %d", w.Code)
	}

	// /hook list never prints the secret
	drain(dc)
	if err := s.RunCommand(dc, ch.ID, "hook", "list"); err != nil {
		t.Fatal(err)
	}
	if list := noticeText(drain(dc)); !strings.Contains(list, h.ID) || strings.Contains(list, h.Token) {
		t.Fatalf("/hook list = %q", list)
	}

	if err := s.RunCommand(dc, ch.ID, "hook", "revoke "+h.ID); err != nil {
		t.Fatalf("/hook revoke: %v", err)
	}
	before := len(devlogEvents(t, s, ch.ID))
	w = deliverHook(s, h.Token, ghPush, map[string]string{
		"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(ghPush, h.Token)})
	if w.Code != 403 {
		t.Fatalf("a revoked hook gave %d, want 403", w.Code)
	}
	if now := len(devlogEvents(t, s, ch.ID)); now != before {
		t.Fatal("a revoked hook still posted to the channel")
	}
}

func TestHookCommandsNeedOp(t *testing.T) {
	s := testServer(t)
	mika, _ := s.St.CreateUser("mika", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, mika.ID, "member")
	mc := fakeConn(s, mika)
	if err := s.RunCommand(mc, ch.ID, "hook", "new"); err == nil {
		t.Fatal("a plain member minted a hook token")
	}
}

// TestNonMemberCannotActInChannel is the regression test for the hole this
// suite did not have: RunCommand resolved a channel from a client-supplied id
// and never asked whether the caller was in it. A non-member scans as role "",
// which ranks equal to "member", so every member-level command ran for anyone
// holding a channel id — and a kicked client still holds one.
func TestNonMemberCannotActInChannel(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	troll, _ := s.St.CreateUser("troll", "t@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, troll.ID, "member")

	dc, tc := fakeConn(s, dana), fakeConn(s, troll)

	// dana kicks troll. troll's client keeps the channel id it already had.
	if err := s.RunCommand(dc, ch.ID, "kick", "troll"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.St.Since(ch.ID, 0, 500)

	// Every member-level way back into the room has to refuse now.
	for _, cmd := range []struct{ name, args string }{
		{"me", "is still here"},
		{"slap", "dana"},
		{"topic", "owned"},
		{"log", "a line"},
		{"todo", "something"},
		{"bans", ""},
	} {
		if err := s.RunCommand(tc, ch.ID, cmd.name, cmd.args); err == nil {
			t.Errorf("/%s ran for a non-member", cmd.name)
		} else if !strings.Contains(err.Error(), "not in that channel") {
			t.Errorf("/%s refused for the wrong reason: %v", cmd.name, err)
		}
	}

	// message.send was already guarded; assert it stays that way.
	if err := s.handleSend(tc, ch.ID, "hello", "", false, 0, "", "", false); err == nil {
		t.Error("a non-member sent a message")
	}

	if after, _ := s.St.Since(ch.ID, 0, 500); len(after) != len(before) {
		t.Fatalf("a non-member wrote %d events into the channel", len(after)-len(before))
	}
}

// TestPartOtherChannelNeedsMembership: /part takes a NAME, so it reached any
// channel on the server by guessing one. Leave was a no-op for a non-member but
// the "*** x has left" event was posted anyway.
func TestPartOtherChannelNeedsMembership(t *testing.T) {
	s := testServer(t)
	mallory, _ := s.St.CreateUser("mallory", "m@x.io", "hunter2hunter2", "member")
	execs, _ := s.St.CreateChannel("execs", "private", false)
	general, _ := s.St.ChannelByName("general")
	s.St.Join(general.ID, mallory.ID, "member")

	mc := fakeConn(s, mallory)
	before, _ := s.St.Since(execs.ID, 0, 100)
	if err := s.RunCommand(mc, general.ID, "part", "#execs"); err == nil {
		t.Fatal("/part reached a channel the caller was never in")
	}
	if after, _ := s.St.Since(execs.ID, 0, 100); len(after) != len(before) {
		t.Fatal("/part posted into a channel the caller was never in")
	}
}

// TestBanSurvivesRename: a ban used to be stored against a nick, and /nick is
// one word, so evading one was one word.
func TestBanSurvivesRename(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	troll, _ := s.St.CreateUser("troll", "t@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, troll.ID, "member")

	dc, tc := fakeConn(s, dana), fakeConn(s, troll)
	if err := s.RunCommand(dc, ch.ID, "ban", "troll spam"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(tc, "", "nick", "evader"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(tc, "", "join", "#general"); err == nil {
		t.Fatal("a ban was evaded by renaming")
	}
}

// TestInviteIsSpentByTheJoin: an invite grant used to live forever, so a kick
// from a +i channel lasted until the kicked user typed /join.
func TestInviteIsSpentByTheJoin(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.CreateChannel("secret", "", false)
	s.St.SetModes(ch.ID, "i")
	ch, _ = s.St.ChannelByID(ch.ID)
	s.St.Join(ch.ID, dana.ID, "op")

	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	if err := s.RunCommand(dc, ch.ID, "invite", "robin"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(rc, "", "join", "#secret"); err != nil {
		t.Fatalf("invited user could not join: %v", err)
	}
	if err := s.RunCommand(dc, ch.ID, "kick", "robin"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(rc, "", "join", "#secret"); err == nil {
		t.Fatal("a kicked user walked back in on a spent invite")
	}
}

// TestUploadOwnershipSurvivesRename: ownership was a nick comparison, so
// releasing a nick handed an in-flight upload to whoever claimed it next.
func TestUploadOwnershipSurvivesRename(t *testing.T) {
	s := testServer(t)
	alice, _ := s.St.CreateUser("alice", "a@x.io", "hunter2hunter2", "member")
	mallory, _ := s.St.CreateUser("mallory", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, alice.ID, "member")

	up, err := s.St.CreateUpload(ch.ID, alice.ID, alice.Nick, "notes.txt", 10,
		filepath.Join(t.TempDir(), "blob"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// alice frees her nick; mallory takes it.
	if err := s.St.SetNick(alice.ID, "alice2"); err != nil {
		t.Fatal(err)
	}
	if err := s.St.SetNick(mallory.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	got, err := s.St.Upload(up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != alice.ID {
		t.Fatalf("upload owner = %q, want %q", got.OwnerID, alice.ID)
	}
	if got.OwnerID == mallory.ID {
		t.Fatal("renaming handed the upload to another account")
	}
}

// TestSessionExpires: a token used to be good forever.
func TestSessionExpires(t *testing.T) {
	s := testServer(t)
	u, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	tok, _, err := s.St.Login("dana", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.UserByToken(tok); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	// Age it past the cap. Nothing else in the row changes.
	old := time.Now().UTC().Add(-sessionTTL - time.Hour).Format(time.RFC3339)
	if _, err := s.St.db.Exec(`UPDATE sessions SET created_at=? WHERE token=?`, old, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.UserByToken(tok); err == nil {
		t.Fatal("an expired session still authenticates")
	}
	s.St.PruneSessions()
	var n int
	s.St.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, u.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d expired sessions survived the prune", n)
	}
}

// Presence rides on the member list: a socket opening or closing refreshes the
// channel for everyone in it, with `online` set from the socket registry.
func TestMemberListCarriesPresence(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")

	rc := fakeConn(s, robin)
	drainCh(rc.out)
	dc := fakeConn(s, dana)

	online := func() map[string]bool {
		out := map[string]bool{}
		for _, m := range s.channelView(ch, robin.ID)["members"].([]Member) {
			out[m.Nick] = m.Online
		}
		return out
	}
	if got := online(); !got["dana"] || !got["robin"] {
		t.Fatalf("both hold a socket, both must read online: %v", got)
	}
	s.flushPresence() // what the presenceDelay timer does
	var pushed bool
	for _, f := range drainCh(rc.out) {
		if f.T == "state" {
			pushed = true
		}
	}
	if !pushed {
		t.Fatal("robin's rail was not refreshed when dana arrived")
	}

	s.removeConn(dc)
	if got := online(); got["dana"] {
		t.Fatalf("dana closed her last socket and still reads online: %v", got)
	}
}

// A reply's target is an id the server hands back, so the clients can jump to
// it. The quote itself is in the body and never seen here — in a +e channel it
// is ciphertext.
func TestReplyToIsRecordedAndScopedToItsChannel(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	other, _ := s.St.CreateChannel("elsewhere", dana.ID, false)
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(other.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	if err := s.handleSend(dc, ch.ID, "the freeze is on for friday", "", false, 0, "", "", false); err != nil {
		t.Fatal(err)
	}
	first, _ := s.St.LastMessage(ch.ID)

	replyTo := func(cid, target string) *Event {
		if err := s.handleSend(dc, cid, "> <dana> the freeze is on for friday\nworks for me", "", false, 0, target, "", false); err != nil {
			t.Fatal(err)
		}
		e, _ := s.St.LastMessage(cid)
		return e
	}

	if got := replyTo(ch.ID, first.ID).Meta["reply_to"]; got != first.ID {
		t.Fatalf("reply_to = %v, want %q", got, first.ID)
	}
	if e := replyTo(ch.ID, "e_nope"); e.Meta["reply_to"] != nil {
		t.Fatalf("an id that is not an event was recorded: %v", e.Meta)
	}
	// The message still posts; it just loses the jump.
	if e := replyTo(other.ID, first.ID); e.Meta["reply_to"] != nil {
		t.Fatalf("a reply pointed across channels: %v", e.Meta)
	}
}

// TestCreateRejectsExisting: /create is /join's create half — it must fail on a
// name that is already taken rather than joining the existing channel.
func TestCreateRejectsExisting(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	dc := fakeConn(s, dana)

	if err := s.RunCommand(dc, "", "create", "#lab"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ch, err := s.St.ChannelByName("lab")
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := s.St.MemberRole(ch.ID, dana.ID); !ok || role != "op" {
		t.Fatalf("creator role = %q, %v; want op", role, ok)
	}
	if err := s.RunCommand(dc, "", "create", "#lab"); err == nil {
		t.Fatal("/create walked into an existing channel")
	}
	if err := s.RunCommand(dc, "", "create", "#general"); err == nil {
		t.Fatal("/create clobbered a channel it is not a member of")
	}
}

// TestInviteSurvivesRestart: a +i grant used to live in a map on Server, so
// every deploy dropped every pending invitation and the invitee was locked out
// of a channel they had been invited to.
func TestInviteSurvivesRestart(t *testing.T) {
	path := testDSN(t)
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(st, "test")
	dana, _ := st.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := st.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := st.CreateChannel("secret", "", false)
	st.SetModes(ch.ID, "i")
	st.Join(ch.ID, dana.ID, "op")
	if err := s.RunCommand(fakeConn(s, dana), ch.ID, "invite", "robin"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// the deploy
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	s2 := NewServer(st2, "test")
	if err := s2.RunCommand(fakeConn(s2, robin), "", "join", "#secret"); err != nil {
		t.Fatalf("invite did not survive the restart: %v", err)
	}
	// and the lock still holds for everyone else
	mal, _ := st2.CreateUser("mallory", "m@x.io", "hunter2hunter2", "member")
	if err := s2.RunCommand(fakeConn(s2, mal), "", "join", "#secret"); err == nil {
		t.Fatal("an uninvited user joined a +i channel")
	}
}

// TestUninviteRevokesTheGrant: a durable invite needs a way back out. /kick on
// someone who has not walked in yet (and its /uninvite alias) spends the grant.
func TestUninviteRevokesTheGrant(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.CreateChannel("secret", "", false)
	s.St.SetModes(ch.ID, "i")
	s.St.Join(ch.ID, dana.ID, "op")
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)

	for _, undo := range []string{"kick", "uninvite"} {
		if err := s.RunCommand(dc, ch.ID, "invite", "robin"); err != nil {
			t.Fatal(err)
		}
		if err := s.RunCommand(dc, ch.ID, undo, "robin"); err != nil {
			t.Fatalf("/%s on a pending invite: %v", undo, err)
		}
		if err := s.RunCommand(rc, "", "join", "#secret"); err == nil {
			t.Fatalf("/%s left the invitation standing", undo)
		}
	}
	// a stranger with no invite is still an error, not a silent no-op
	if err := s.RunCommand(dc, ch.ID, "kick", "robin"); err == nil {
		t.Fatal("/kick on a non-member with no invite should fail")
	}
}

// An op adds a connected plugin to a channel with /invite; until then its
// commands are refused there, and a plain member cannot add it.
func TestInvitePluginIntoChannel(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "admin")
	sam, _ := s.St.CreateUser("sam", "s@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.CreateChannel("agents", "", false)
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, sam.ID, "member")
	dc, sc := fakeConn(s, dana), fakeConn(s, sam)
	meta, _ := s.St.CreatePlugin("substrate", "substrate", "agent")
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 64)}
	s.mu.Lock()
	s.plugins[meta.ID] = p
	s.mu.Unlock()
	if err := s.handlePluginFrame(p, []byte(`{"t":"register","d":{"display":"substrate","commands":[{"name":"substrate","desc":"x","scope":"channel"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RunCommand(dc, ch.ID, "substrate", "help"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("plugin command ran before the plugin was added: %v", err)
	}
	if err := s.RunCommand(sc, ch.ID, "invite", "substrate"); err == nil {
		t.Fatal("a plain member added a plugin to the channel")
	}
	if err := s.RunCommand(dc, ch.ID, "invite", "substrate"); err != nil {
		t.Fatalf("op could not add the plugin: %v", err)
	}
	if !s.St.PluginInChannel(meta.ID, ch.ID) {
		t.Fatal("plugin is not a member after /invite")
	}
	if err := s.RunCommand(dc, ch.ID, "substrate", "help"); err != nil {
		t.Fatalf("plugin command refused after /invite: %v", err)
	}
	// The op can take it out again.
	if err := s.RunCommand(dc, ch.ID, "kick", "substrate"); err != nil {
		t.Fatalf("op could not remove the plugin: %v", err)
	}
	if s.St.PluginInChannel(meta.ID, ch.ID) {
		t.Fatal("plugin still a member after /kick")
	}
	// Its own subscribe list does not bring it back.
	if err := s.handlePluginFrame(p, []byte(`{"t":"register","d":{"commands":[{"name":"substrate","desc":"x","scope":"channel"}],"subscribe":["`+ch.ID+`"]}}`)); err != nil {
		t.Fatal(err)
	}
	if s.St.PluginInChannel(meta.ID, ch.ID) {
		t.Fatal("a kicked plugin re-subscribed itself")
	}
	// Two plugins under one name: refused, the id still works.
	meta2, _ := s.St.CreatePlugin("impostor", "impostor", "agent")
	p2 := &PluginConn{srv: s, meta: meta2, out: make(chan Frame, 64)}
	s.mu.Lock()
	s.plugins[meta2.ID] = p2
	s.mu.Unlock()
	s.mu.Lock()
	p.meta.Display, p2.meta.Display = "relaybot", "relaybot"
	s.mu.Unlock()
	if err := s.RunCommand(dc, ch.ID, "invite", "relaybot"); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("ambiguous plugin name was not refused: %v", err)
	}
	if err := s.RunCommand(dc, ch.ID, "invite", meta.ID); err != nil || !s.St.PluginInChannel(meta.ID, ch.ID) {
		t.Fatalf("invite by id failed: %v", err)
	}
}

// A plugin may name a known agent brand on a line it posts, and nothing else.
func TestPluginAgentBrandMeta(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	meta, _ := s.St.CreatePlugin("relay", "relay", "agent")
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 64)}
	s.mu.Lock()
	s.plugins[meta.ID] = p
	s.mu.Unlock()
	s.St.PluginJoin(meta.ID, ch.ID)
	for _, tc := range []struct{ agent, want string }{{"claude", "claude"}, {"https://evil.example/x.svg", ""}, {"", ""}} {
		b, _ := json.Marshal(map[string]any{"t": "say", "d": map[string]string{"channel": ch.ID, "body": "hi", "agent": tc.agent}})
		f := string(b)
		if err := s.handlePluginFrame(p, []byte(f)); err != nil {
			t.Fatal(err)
		}
		evs, _ := s.St.Since(ch.ID, 0, 1000)
		e := evs[len(evs)-1]
		got, _ := e.Meta["agent"].(string)
		if got != tc.want {
			t.Fatalf("agent %q stored as %q, want %q", tc.agent, got, tc.want)
		}
	}
}

// A plugin that connects again replaces its old connection, and the old
// socket is closed: its client must see that and not sit on a dead line.
func TestPluginReconnectClosesTheOldSocket(t *testing.T) {
	s := testServer(t)
	meta, _ := s.St.CreatePlugin("relay", "relay", "agent")
	srv := httptest.NewServer(s.Routes("", t.TempDir()))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/plugin?token=" + meta.Token
	ctx := context.Background()
	first, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseNow()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, _, err := first.Read(rctx); err == nil || rctx.Err() != nil {
		t.Fatalf("old plugin socket stayed open after a newer connection (err=%v)", err)
	}
	// The newer connection keeps what it registers after the old one's cleanup ran.
	if err := second.Write(ctx, websocket.MessageText, []byte(`{"t":"register","d":{"commands":[{"name":"relaycmd","desc":"x"}]}}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := s.Reg.Lookup("relaycmd"); !ok {
		t.Fatal("the newer connection's command was removed by the old connection's cleanup")
	}
	if len(s.ConnectedPlugins()) != 1 {
		t.Fatalf("connected plugins = %d, want 1", len(s.ConnectedPlugins()))
	}
}
