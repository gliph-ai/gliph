package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mintedToken(t *testing.T, s *Server, cid, txt string) Hook {
	t.Helper()
	hooks, _ := s.St.Hooks(cid)
	for _, h := range hooks {
		if strings.Contains(txt, h.Token) {
			return h
		}
	}
	t.Fatalf("no minted token was printed: %s", txt)
	return Hook{}
}

func postIn(s *Server, tok, ctype, body string) int {
	mux := http.NewServeMux()
	s.DevlogRoutes(mux)
	r := httptest.NewRequest("POST", "/api/hooks/in/"+tok, strings.NewReader(body))
	r.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

// An incoming hook posts into its one channel, under its name, as an agent
// line. Its token is scoped: it opens nothing else, and revoking it shuts it.
func TestIncomingWebhookPostsAsALabeledIntegration(t *testing.T) {
	s, ch, dc, gitTok := devlogSetup(t)
	s.St.CreateUser("waseem", "w@x.io", "hunter2hunter2", "member")
	if err := s.RunCommand(dc, ch.ID, "hook", "in waseem"); err == nil {
		t.Fatal("a hook took a member's nick")
	}
	if err := s.RunCommand(dc, ch.ID, "hook", "in ci-bot"); err != nil {
		t.Fatalf("/hook in: %v", err)
	}
	h := mintedToken(t, s, ch.ID, noticeText(drain(dc)))
	if h.Kind != "in" || h.Name != "ci-bot" {
		t.Fatalf("hook = %+v", h)
	}

	if c := postIn(s, h.Token, "application/json", `{"text":"build 412 is green"}`); c != 200 {
		t.Fatalf("json post = %d", c)
	}
	if c := postIn(s, h.Token, "text/plain", "deploy done"); c != 200 {
		t.Fatalf("text post = %d", c)
	}
	evs, _ := s.St.Since(ch.ID, 0, 100)
	var got []string
	for _, e := range evs {
		if e.Kind == "agent" {
			if e.Actor != "ci-bot" || e.Meta["webhook"] != h.ID {
				t.Fatalf("line not labeled as the integration: %+v", e)
			}
			got = append(got, e.Body)
		}
	}
	if strings.Join(got, "|") != "build 412 is green|deploy done" {
		t.Fatalf("posted %v", got)
	}

	// scoped: the git token does not post here, nor this one at the git URL
	if c := postIn(s, gitTok, "text/plain", "x"); c != 403 {
		t.Fatalf("git token accepted at the incoming endpoint: %d", c)
	}
	if w := deliverHook(s, h.Token, ghPush, map[string]string{"X-GitHub-Event": "push",
		"X-Hub-Signature-256": githubSig(ghPush, h.Token)}); w.Code != 403 {
		t.Fatalf("incoming token accepted at the git endpoint: %d", w.Code)
	}
	s.St.DeleteHook(ch.ID, h.ID)
	if c := postIn(s, h.Token, "text/plain", "after revoke"); c != 403 {
		t.Fatalf("revoked hook still posts: %d", c)
	}
}

// An outgoing hook delivers every event, signed with its secret. In a +e
// channel the delivery carries the metadata and never the body.
func TestOutgoingWebhookIsSignedAndKeepsCiphertextOut(t *testing.T) {
	got := make(chan *http.Request, 8)
	bodies := make(chan []byte, 8)
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- b
	}))
	defer rx.Close()
	old := hookAllowed
	hookAllowed = func(net.IP) bool { return true } // the receiver is on loopback
	defer func() { hookAllowed = old }()

	s, ch, dc, _ := devlogSetup(t)
	if err := s.RunCommand(dc, ch.ID, "hook", "out "+rx.URL); err != nil {
		t.Fatalf("/hook out: %v", err)
	}
	h := mintedToken(t, s, ch.ID, noticeText(drain(dc)))
	// the "added an outgoing webhook" line is itself an event; take it first
	next := func() (map[string]any, *http.Request) {
		t.Helper()
		select {
		case r := <-got:
			b := <-bodies
			if r.Header.Get("X-Gliph-Signature") != hookSignature(b, h.Token) {
				t.Fatalf("bad signature on %s", b)
			}
			var p map[string]any
			json.Unmarshal(b, &p)
			return p["event"].(map[string]any), r
		case <-time.After(3 * time.Second):
			t.Fatal("no delivery")
		}
		return nil, nil
	}
	next()

	s.Post(ch, &Event{Kind: "message", Actor: "dana", Body: "plain words"}, "")
	ev, r := next()
	if ev["body"] != "plain words" || r.Header.Get("X-Gliph-Event") != "message" {
		t.Fatalf("delivery = %v", ev)
	}

	ch.Modes = "nte"
	s.Post(ch, &Event{Kind: "message", Actor: "dana", Body: "Q0lQSEVS", Enc: true, Epoch: 1}, "")
	ev, _ = next()
	if _, has := ev["body"]; has || ev["enc"] != true || ev["actor"] != "dana" {
		t.Fatalf("+e delivery leaked a body or lost its metadata: %v", ev)
	}

	// an incoming hook's line is not echoed back out
	s.Post(ch, &Event{Kind: "agent", Actor: "ci", Body: "x", Meta: map[string]any{"webhook": "h_1"}}, "")
	select {
	case <-got:
		t.Fatal("a webhook line was sent back out")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOutgoingWebhookRefusesPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "169.254.169.254", "192.168.1.1", "::1", "100.64.0.1"} {
		if hookAllowed(net.ParseIP(ip)) {
			t.Errorf("%s allowed", ip)
		}
	}
	if !hookAllowed(net.ParseIP("1.1.1.1")) {
		t.Error("a public address was refused")
	}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:1/", bytes.NewReader(nil))
	if _, err := hookClient.Do(req); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("dial to loopback = %v", err)
	}
}
