package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A recording sender, standing in for the relay.
type capturedWake struct{ devices []PushDevice }

type spySender struct{ calls []capturedWake }

func (s *spySender) Wake(d []PushDevice) { s.calls = append(s.calls, capturedWake{devices: d}) }

// The load-bearing test for F5: whatever goes over the wire to wake a phone
// must not contain a single readable thing about the message that caused it.
func TestWakePayloadCarriesNoContent(t *testing.T) {
	s := testServer(t)
	spy := &spySender{}
	s.Push = newPushHub(s.St, spy)

	sender, _ := s.St.CreateUser("sender", "s@x", "password1", "member")
	away, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	if err := s.St.RegisterPushDevice(away.ID, "ios", "device-token-aaa"); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.St.CreateChannel("secrets", "", false)
	s.St.Join(ch.ID, sender.ID, "op")
	s.St.Join(ch.ID, away.ID, "member")

	const secret = "the eagle lands at midnight"
	if err := s.Post(ch, &Event{Kind: "message", Actor: sender.Nick, Body: secret}, ""); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("expected one wake for the offline member, got %d", len(spy.calls))
	}

	// Serialise exactly what the relay sender would send and search it.
	ws := []wake{}
	for _, d := range spy.calls[0].devices {
		ws = append(ws, wake{Platform: d.Platform, Token: d.Token})
	}
	body, err := json.Marshal(map[string]any{"wake": ws})
	if err != nil {
		t.Fatal(err)
	}
	payload := string(body)
	for _, leak := range []string{secret, "eagle", sender.Nick, "secrets", ch.ID, ch.Name} {
		if strings.Contains(payload, leak) {
			t.Fatalf("push payload leaked %q: %s", leak, payload)
		}
	}
	if !strings.Contains(payload, "device-token-aaa") {
		t.Fatalf("wake did not address the device: %s", payload)
	}
}

// The desktop app being open is not a reason to leave the phone silent — it is
// the most common reason a DM used to arrive on the phone with no notification
// at all. The sender's own devices stay quiet; everyone else's phone rings.
func TestADesktopSocketDoesNotSilenceThePhone(t *testing.T) {
	s := testServer(t)
	spy := &spySender{}
	s.Push = newPushHub(s.St, spy)

	sender, _ := s.St.CreateUser("sender", "s@x", "password1", "member")
	here, _ := s.St.CreateUser("here", "h@x", "password1", "member")
	s.St.RegisterPushDevice(sender.ID, "ios", "tok-sender")
	s.St.RegisterPushDevice(here.ID, "ios", "tok-here")
	ch, _ := s.St.CreateChannel("room", "", false)
	s.St.Join(ch.ID, sender.ID, "op")
	s.St.Join(ch.ID, here.ID, "member")

	// Both of them have the desktop app open.
	for _, u := range []*User{sender, here} {
		c := &Conn{srv: s, user: u, client: "desktop/0.1.0", out: make(chan Frame, 8)}
		if !s.addConn(c) {
			t.Fatal("could not register the connection")
		}
		defer s.removeConn(c)
	}

	s.Post(ch, &Event{Kind: "message", Actor: sender.Nick, Body: "hello"}, "")
	if len(spy.calls) != 1 {
		t.Fatalf("expected the recipient's phone to be woken, got %d wakes", len(spy.calls))
	}
	if len(spy.calls[0].devices) != 1 || spy.calls[0].devices[0].Token != "tok-here" {
		t.Fatalf("woke the wrong devices: %+v", spy.calls[0].devices)
	}
}

// A burst is one reason to pick up the phone. Both platforms throttle a server
// that disagrees.
func TestWakesAreCoalescedPerUser(t *testing.T) {
	s := testServer(t)
	spy := &spySender{}
	s.Push = newPushHub(s.St, spy)

	sender, _ := s.St.CreateUser("sender", "s@x", "password1", "member")
	away, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	s.St.RegisterPushDevice(away.ID, "android", "tok-away")
	ch, _ := s.St.CreateChannel("busy", "", false)
	s.St.Join(ch.ID, sender.ID, "op")
	s.St.Join(ch.ID, away.ID, "member")

	for i := 0; i < 20; i++ {
		s.Post(ch, &Event{Kind: "message", Actor: sender.Nick, Body: "spam"}, "")
	}
	if len(spy.calls) != 1 {
		t.Fatalf("20 messages produced %d wakes, want 1", len(spy.calls))
	}

	// Housekeeping events are not worth a buzz at all.
	s.Push = newPushHub(s.St, spy)
	spy.calls = nil
	s.Post(ch, &Event{Kind: "join", Actor: sender.Nick, Body: ""}, "")
	s.Post(ch, &Event{Kind: "mode", Actor: sender.Nick, Body: "+m"}, "")
	if len(spy.calls) != 0 {
		t.Fatalf("housekeeping events woke a phone: %+v", spy.calls)
	}
}

// Registration is idempotent, and a token never comes back out of the API.
func TestPushRegistrationIsIdempotentAndTokensAreWriteOnly(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	u, _ := s.St.CreateUser("nick", "n@x", "password1", "member")
	tok, _, err := s.St.Login("nick", "password1")
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/push/devices", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	for i := 0; i < 3; i++ {
		if w := do("POST", `{"platform":"ios","token":"same-token"}`); w.Code != 200 {
			t.Fatalf("register %d: %d %s", i, w.Code, w.Body)
		}
	}
	devs, _ := s.St.PushDevices(u.ID)
	if len(devs) != 1 {
		t.Fatalf("re-registering the same token made %d rows", len(devs))
	}

	// The serialised device must not carry the token.
	b, _ := json.Marshal(devs[0])
	if strings.Contains(string(b), "same-token") {
		t.Fatalf("device JSON leaked the push token: %s", b)
	}

	if w := do("POST", `{"platform":"desktop","token":"x"}`); w.Code != 400 {
		t.Fatalf("accepted an unknown platform: %d", w.Code)
	}

	if w := do("DELETE", `{"token":"same-token"}`); w.Code != 200 {
		t.Fatalf("deregister: %d %s", w.Code, w.Body)
	}
	if devs, _ := s.St.PushDevices(u.ID); len(devs) != 0 {
		t.Fatalf("device survived deregistration: %+v", devs)
	}
}
