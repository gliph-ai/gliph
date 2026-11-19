package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Drives the whole flow the way a real client would, then attacks it.
func TestDeviceLinkFlow(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	s.St.CreateUser("waseem", "w@x", "password1", "admin")
	sess, _, err := s.St.Login("waseem", "password1")
	if err != nil {
		t.Fatal(err)
	}

	post := func(path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	const verifier = "a-verifier-long-enough-to-be-worth-something-0123456789"
	const redirect = "http://127.0.0.1:51234/cb"
	authBody := func(rd, ch, method string) string {
		b, _ := json.Marshal(map[string]string{
			"device_name": "Waseem's laptop", "platform": "desktop",
			"redirect_uri": rd, "code_challenge": ch, "code_challenge_method": method,
		})
		return string(b)
	}

	// --- the approval step is a signed-in human, and only that ---
	if w := post("/api/auth/device-link/authorize", authBody(redirect, challengeFor(verifier), "S256"), ""); w.Code != 401 {
		t.Fatalf("unauthenticated approval returned %d, want 401", w.Code)
	}

	// --- plain PKCE is refused: it would put the verifier in a URL ---
	if w := post("/api/auth/device-link/authorize", authBody(redirect, challengeFor(verifier), "plain"), sess); w.Code != 400 {
		t.Fatalf("accepted code_challenge_method=plain: %d", w.Code)
	}

	// --- THE ONE THAT MATTERS: a non-loopback redirect must never be minted ---
	for _, bad := range []string{
		"https://evil.example/cb",
		"http://evil.example/cb",
		"http://127.0.0.1.evil.example/cb",
		"http://localhost:9/cb", // resolves through the host's name service; not ours to assume
	} {
		w := post("/api/auth/device-link/authorize", authBody(bad, challengeFor(verifier), "S256"), sess)
		if w.Code != 400 {
			t.Fatalf("minted a code redirecting to %q (%d) — that is an exfiltration channel", bad, w.Code)
		}
	}

	// --- the happy path ---
	w := post("/api/auth/device-link/authorize", authBody(redirect, challengeFor(verifier), "S256"), sess)
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body)
	}
	var minted struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &minted)
	if minted.Code == "" {
		t.Fatal("no code returned")
	}

	tokenBody := func(code, ver, rd string) string {
		b, _ := json.Marshal(map[string]string{"code": code, "code_verifier": ver, "redirect_uri": rd})
		return string(b)
	}

	// --- a thief who caught the code but has no verifier gets nothing ---
	if w := post("/api/auth/device-link/token", tokenBody(minted.Code, "not-the-verifier", redirect), ""); w.Code != 400 {
		t.Fatalf("redeemed a code without the verifier: %d %s", w.Code, w.Body)
	}
	// ...and that attempt SPENT the code. No second guess with the same one.
	if w := post("/api/auth/device-link/token", tokenBody(minted.Code, verifier, redirect), ""); w.Code != 400 {
		t.Fatalf("a code survived a failed redemption: %d", w.Code)
	}

	// --- a fresh code, redeemed properly ---
	w = post("/api/auth/device-link/authorize", authBody(redirect, challengeFor(verifier), "S256"), sess)
	json.Unmarshal(w.Body.Bytes(), &minted)

	w = post("/api/auth/device-link/token", tokenBody(minted.Code, verifier, redirect), "")
	if w.Code != 200 {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	var got struct {
		Token string
		User  struct{ Nick string }
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Token == "" || got.User.Nick != "waseem" {
		t.Fatalf("bad redemption result: %s", w.Body)
	}
	// The minted token really is a working session.
	if u, err := s.St.UserByToken(got.Token); err != nil || u.Nick != "waseem" {
		t.Fatalf("minted token does not authenticate: %v", err)
	}
	// Single use, still.
	if w := post("/api/auth/device-link/token", tokenBody(minted.Code, verifier, redirect), ""); w.Code != 400 {
		t.Fatalf("code was redeemable twice: %d", w.Code)
	}
}

// Listing your devices must not hand a stolen session the other devices'
// tokens, and revoking one must not touch the rest.
func TestLinkedDevicesAreListableAndRevocableWithoutLeakingTokens(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	u, _ := s.St.CreateUser("waseem", "w@x", "password1", "member")
	browser, _, _ := s.St.Login("waseem", "password1")

	laptopTok, err := s.St.NewDeviceSession(u.ID, "Laptop", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	phoneTok, _ := s.St.NewDeviceSession(u.ID, "Phone", "ios")

	get := func(method, path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := get("GET", "/api/auth/devices", browser)
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	body := w.Body.String()
	for _, tok := range []string{laptopTok, phoneTok, browser} {
		if strings.Contains(body, tok) {
			t.Fatalf("device listing leaked a session token: %s", body)
		}
	}
	var list []LinkedDevice
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("want 2 linked devices (the browser login is not one), got %d: %s", len(list), body)
	}

	// Revoke the phone; the laptop and the browser keep working.
	var phoneHandle string
	for _, d := range list {
		if d.Name == "Phone" {
			phoneHandle = d.ID
		}
	}
	if w := get("DELETE", "/api/auth/devices/"+phoneHandle, browser); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.UserByToken(phoneTok); err == nil {
		t.Fatal("revoked device still authenticates")
	}
	if _, err := s.St.UserByToken(laptopTok); err != nil {
		t.Fatal("revoking one device signed out another")
	}
	if _, err := s.St.UserByToken(browser); err != nil {
		t.Fatal("revoking a device signed out the browser")
	}
}

// One user must never be able to revoke another's device, even knowing the
// handle.
func TestOneUserCannotRevokeAnothersDevice(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	victim, _ := s.St.CreateUser("victim", "v@x", "password1", "member")
	s.St.CreateUser("thief", "t@x", "password1", "member")
	victimDev, _ := s.St.NewDeviceSession(victim.ID, "Laptop", "desktop")
	thiefSess, _, _ := s.St.Login("thief", "password1")

	list, _ := s.St.LinkedDevices(victim.ID, "")
	if len(list) != 1 {
		t.Fatal("setup")
	}

	r := httptest.NewRequest("DELETE", "/api/auth/devices/"+list[0].ID, nil)
	r.Header.Set("Authorization", "Bearer "+thiefSess)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("one user revoked another user's device")
	}
	if _, err := s.St.UserByToken(victimDev); err != nil {
		t.Fatal("the victim's device was signed out")
	}
}

// Substrate is the one web client: its code goes only to the callback this
// server was started with, and never anywhere when none is configured.
func TestDeviceLinkSubstrateCallback(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())
	s.St.CreateUser("waseem", "w@x", "password1", "admin")
	sess, _, _ := s.St.Login("waseem", "password1")
	authorize := func(rd string) int {
		b, _ := json.Marshal(map[string]string{"device_name": "Substrate", "platform": "substrate", "redirect_uri": rd,
			"code_challenge": challengeFor("a-verifier-long-enough-to-be-worth-something-0123456789"), "code_challenge_method": "S256"})
		r := httptest.NewRequest("POST", "/api/auth/device-link/authorize", strings.NewReader(string(b)))
		r.Header.Set("Authorization", "Bearer "+sess)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	const cb = "https://substrate.gliph.sh/auth/gliph/callback"
	if c := authorize(cb); c != 400 {
		t.Fatalf("no -substrate-callback configured, yet minted a code (%d)", c)
	}
	s.SubstrateCallback = cb
	for _, bad := range []string{"https://evil.example/auth/gliph/callback", cb + "?x=1", "https://substrate.gliph.sh/other", "http://127.0.0.1:5000/cb"} {
		if c := authorize(bad); c != 400 {
			t.Fatalf("substrate platform minted a code for %q (%d)", bad, c)
		}
	}
	if c := authorize(cb); c != 200 {
		t.Fatalf("the configured callback was refused (%d)", c)
	}
	// Redeemed, the substrate code carries the email (and only it does).
	b, _ := json.Marshal(map[string]string{"device_name": "Substrate", "platform": "substrate", "redirect_uri": cb,
		"code_challenge": challengeFor("a-verifier-long-enough-to-be-worth-something-0123456789"), "code_challenge_method": "S256"})
	r := httptest.NewRequest("POST", "/api/auth/device-link/authorize", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+sess)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var minted struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &minted)
	tb, _ := json.Marshal(map[string]string{"code": minted.Code, "code_verifier": "a-verifier-long-enough-to-be-worth-something-0123456789", "redirect_uri": cb})
	r = httptest.NewRequest("POST", "/api/auth/device-link/token", strings.NewReader(string(tb)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got struct{ Email string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Email != "w@x" {
		t.Fatalf("substrate token response email = %q (%d %s)", got.Email, w.Code, w.Body)
	}
}
