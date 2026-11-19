package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The TURN credential has to be one coturn will actually accept, and it must
// not carry anything about who asked for it.
func TestTurnCredentialIsEphemeralAndAnonymous(t *testing.T) {
	s := testServer(t)
	s.ICE = ICEConfig{
		STUN:   "stun:stun.example.com:3478",
		TURN:   "turn:turn.example.com:3478?transport=udp, turns:turn.example.com:5349",
		Secret: "a-static-auth-secret",
	}
	h := s.Routes("", t.TempDir())

	s.St.CreateUser("waseem", "w@x", "password1", "member")
	tok, _, err := s.St.Login("waseem", "password1")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/ice", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("ice: %d %s", w.Code, w.Body)
	}
	// A credential that expires must never be cached.
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control is %q — a cached credential outlives itself", cc)
	}

	var got struct {
		Servers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"ice_servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 2 {
		t.Fatalf("want a STUN entry and a TURN entry, got %d: %s", len(got.Servers), w.Body)
	}

	turn := got.Servers[1]
	if len(turn.URLs) != 2 {
		t.Fatalf("comma-separated TURN URLs were not split: %+v", turn.URLs)
	}

	// The relay's own logs must not learn who was transferring.
	if strings.Contains(w.Body.String(), "waseem") {
		t.Fatalf("ICE response names the user — that is a social graph in the relay's logs: %s", w.Body)
	}

	// coturn: username is "<expiry>:<anything>", credential is
	// base64(HMAC-SHA1(secret, username)). Verify the way coturn would.
	expiry, rest, ok := strings.Cut(turn.Username, ":")
	if !ok || rest == "" {
		t.Fatalf("username is not <expiry>:<name>: %q", turn.Username)
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		t.Fatalf("expiry is not a unix time: %q", expiry)
	}
	ttl := time.Until(time.Unix(unix, 0))
	if ttl <= 0 || ttl > 24*time.Hour {
		t.Fatalf("credential TTL is %v — want a few hours, not forever and not the past", ttl)
	}

	mac := hmac.New(sha1.New, []byte("a-static-auth-secret"))
	mac.Write([]byte(turn.Username))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); turn.Credential != want {
		t.Fatalf("credential does not verify against the secret:\n got %q\nwant %q", turn.Credential, want)
	}

	// Two requests must not produce the same credential.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/ice", nil)
	r2.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w2, r2)
	if w2.Body.String() == w.Body.String() {
		t.Fatal("the same credential was issued twice")
	}
}

// TURN URLs with no secret would be served as an unusable entry that every
// relay refuses; and an unconfigured server must not hand out anything at all.
func TestIceIsEmptyWithoutConfigAndRequiresAuth(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	s.St.CreateUser("waseem", "w@x", "password1", "member")
	tok, _, _ := s.St.Login("waseem", "password1")

	// Anonymous request: relay bandwidth is not free to strangers.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/ice", nil))
	if w.Code != 401 {
		t.Fatalf("unauthenticated ICE request returned %d, want 401", w.Code)
	}

	get := func() string {
		r := httptest.NewRequest("GET", "/api/ice", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}

	if body := get(); !strings.Contains(body, `"ice_servers":[]`) {
		t.Fatalf("an unconfigured server offered ICE servers: %s", body)
	}

	// TURN with no secret is silently dropped rather than served unusable.
	s.ICE = ICEConfig{TURN: "turn:turn.example.com:3478"}
	if body := get(); strings.Contains(body, "turn:") {
		t.Fatalf("served a TURN URL with no credential: %s", body)
	}
}
