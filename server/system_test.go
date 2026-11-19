package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The MOTD round-trip an operator actually performs: save it, read it back in
// the shape hello carries, and have the id follow the text.
func TestMOTDSettings(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())

	s.St.CreateUser("boss", "boss@x", "password1", "admin")
	tok, _, err := s.St.Login("boss", "password1")
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Nothing set: hello carries no MOTD at all, so a client has nothing to draw.
	if s.motd() != nil {
		t.Fatalf("a server with no MOTD offered one: %v", s.motd())
	}

	icon := "data:image/png;base64,iVBORw0KGgo="
	if w := do("POST", "/api/admin/settings", `{"motd":"  welcome aboard  ","icon":"`+icon+`"}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	m := s.motd()
	if m["text"] != "welcome aboard" || m["icon"] != icon {
		t.Fatalf("MOTD came back wrong: %v", m)
	}
	first := m["id"]

	// The id is a digest of the text: re-saving the same words must not push the
	// message back into the face of everyone who dismissed it, and editing must.
	do("POST", "/api/admin/settings", `{"motd":"welcome aboard","icon":"`+icon+`"}`)
	if s.motd()["id"] != first {
		t.Fatal("an unchanged MOTD changed its id, so every dismissal was undone")
	}
	do("POST", "/api/admin/settings", `{"motd":"we moved to the new server","icon":""}`)
	if s.motd()["id"] == first {
		t.Fatal("an edited MOTD kept its id, so nobody would see it again")
	}

	// A remote URL as the icon would make every client fetch a stranger's image
	// to draw a line of chat. Refused at the door.
	if w := do("POST", "/api/admin/settings", `{"motd":"hi","icon":"https://example.com/logo.png"}`); w.Code != 400 {
		t.Fatalf("accepted a remote icon: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/admin/settings", `{"motd":"hi","icon":"data:image/png;base64,`+strings.Repeat("A", maxIconBytes)+`"}`); w.Code != 400 {
		t.Fatalf("accepted an oversized icon: %d", w.Code)
	}

	// The console reads back what it saved.
	var got struct{ MOTD, Icon string }
	json.Unmarshal(do("GET", "/api/admin/settings", "").Body.Bytes(), &got)
	if got.MOTD != "we moved to the new server" || got.Icon != "" {
		t.Fatalf("settings read back wrong: %+v", got)
	}
}

// Release notes ship inside the binary, so a broken or renamed file is a build
// that quietly stops telling anyone what changed.
func TestReleasesEmbedded(t *testing.T) {
	if len(releases) == 0 {
		t.Fatal("releases.json parsed to nothing")
	}
	if len(news()) > newsInHello {
		t.Fatalf("hello would carry %d notes", len(news()))
	}
	// The changelog endpoint is public and serves the whole list, which is what
	// a website or a curl reads. hello only ever carries the newest few.
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/releases", nil))
	var got []Release
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("GET /api/releases: %d %v", w.Code, err)
	}
	if len(got) != len(releases) {
		t.Fatalf("the endpoint served %d of %d notes", len(got), len(releases))
	}

	seen := map[string]bool{}
	for i, r := range releases {
		if i > 0 && releases[i-1].Date > r.Date {
			t.Fatalf("notes out of order: %s (%s) after %s (%s)", r.ID, r.Date, releases[i-1].ID, releases[i-1].Date)
		}
		if r.ID == "" || r.Title == "" || r.Body == "" {
			t.Fatalf("incomplete release note: %+v", r)
		}
		if seen[r.ID] {
			t.Fatalf("two release notes share the id %q, so dismissing one hides the other", r.ID)
		}
		seen[r.ID] = true
	}
}
