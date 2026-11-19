package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordSender struct {
	mu  sync.Mutex
	got []PushDevice
}

func (r *recordSender) Wake(ds []PushDevice) {
	r.mu.Lock()
	r.got = append(r.got, ds...)
	r.mu.Unlock()
}

func relayPost(p *pushRelay, ip, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.RemoteAddr = ip + ":5000"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// A self-hosted server's relaySender talks to the relay as written: each
// platform's wake reaches its own sender, and a body field is refused.
func TestRelayForwardsWakesToEachPlatform(t *testing.T) {
	ios, android := &recordSender{}, &recordSender{}
	p := newPushRelay(byPlatform{"ios": ios, "android": android})
	srv := httptest.NewServer(p)
	defer srv.Close()

	s := testServer(t)
	u, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	s.St.RegisterPushDevice(u.ID, "ios", "iostoken")
	s.St.RegisterPushDevice(u.ID, "android", "fcmtoken")
	devs, _ := s.St.PushDevices(u.ID)
	r := NewRelaySender(srv.URL).(*relaySender)
	r.Wake(devs)
	waitFor(t, func() bool {
		ios.mu.Lock()
		android.mu.Lock()
		defer ios.mu.Unlock()
		defer android.mu.Unlock()
		return len(ios.got) == 1 && len(android.got) == 1
	})
	if ios.got[0].Token != "iostoken" || android.got[0].Token != "fcmtoken" {
		t.Fatalf("ios %+v android %+v", ios.got, android.got)
	}

	if rec := relayPost(p, "192.0.2.9", `{"wake":[{"platform":"ios","token":"x","body":"hello"}]}`); rec.Code != 400 {
		t.Fatalf("a wake with a body: %d", rec.Code)
	}
}

// One server cannot spend another's budget, and one device is not woken
// again a moment later however often it is asked.
func TestRelayLimitsEachServer(t *testing.T) {
	ios := &recordSender{}
	p := newPushRelay(byPlatform{"ios": ios})
	batch := func(prefix string) string {
		var ws []string
		for i := 0; i < 100; i++ {
			ws = append(ws, `{"platform":"ios","token":"`+prefix+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`)
		}
		return `{"wake":[` + strings.Join(ws, ",") + `]}`
	}
	for i, want := range []int{200, 200, 200, 429} {
		if rec := relayPost(p, "192.0.2.1", batch(string(rune('a'+i)))); rec.Code != want {
			t.Fatalf("batch %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if n := len(ios.got); n != 300 {
		t.Fatalf("%d wakes went out, want the 300 burst", n)
	}
	// Another server is untouched; so is the same /64 not.
	if rec := relayPost(p, "198.51.100.7", batch("z")); rec.Code != 200 {
		t.Fatalf("second server: %d", rec.Code)
	}
	v6 := func(ip string) int { return relayPost(p, ip, batch("v6"+ip[len(ip)-1:])).Code }
	for i := 0; i < 3; i++ {
		v6("2001:db8::1")
	}
	if c := v6("2001:db8::2"); c != 429 {
		t.Fatalf("same /64: %d", c)
	}

	// The same device three times is woken three times (the burst), then not.
	before := len(ios.got)
	for i := 0; i < 5; i++ {
		relayPost(p, "203.0.113.5", `{"wake":[{"platform":"ios","token":"one"}]}`)
	}
	if n := len(ios.got) - before; n != 3 {
		t.Fatalf("one device woken %d times", n)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200 && !ok(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !ok() {
		t.Fatal("timed out")
	}
}
