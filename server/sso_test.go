package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signAssertion(t *testing.T, priv ed25519.PrivateKey, c ssoClaims) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, raw))
}

func ssoShard(t *testing.T) (*Shard, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShard(ShardOpts{
		Senders: byPlatform{}, Secret: "shhh",
		PG: shardPG(t), DataDir: t.TempDir(), ControlKey: pub,
	})
	t.Cleanup(sh.Close)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	provision(t, sh, "shhh", "hooli", "hooli.gliph.chat")
	return sh, priv
}

func redeem(sh *Shard, host, assertion string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/sso",
		strings.NewReader(`{"assertion":"`+assertion+`"}`))
	r.Host = host
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, r)
	return w
}

func TestSSOMintsASessionForAVouchedAccount(t *testing.T) {
	sh, priv := ssoShard(t)
	a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
		Aud: "acme", Email: "waseem@sadiq.nl", Nick: "waseem",
		Exp: time.Now().Add(time.Minute).Unix(),
	})
	w := redeem(sh, "acme.gliph.chat", a)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Token string                `json:"token"`
		User  struct{ Nick string } `json:"user"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Token == "" || got.User.Nick != "waseem" {
		t.Fatalf("got %+v", got)
	}
	// and the session is an ordinary one, indistinguishable downstream
	u, err := sh.tenantFor("acme.gliph.chat").St.UserByToken(got.Token)
	if err != nil || u.Nick != "waseem" {
		t.Fatalf("the token is not a real session: %v %+v", err, u)
	}
}

// The same email arriving twice is the same person, not a second account.
func TestSSOIsIdempotentForTheSameEmail(t *testing.T) {
	sh, priv := ssoShard(t)
	mint := func() string {
		a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
			Aud: "acme", Email: "waseem@sadiq.nl", Nick: "waseem",
			Exp: time.Now().Add(time.Minute).Unix(),
		})
		w := redeem(sh, "acme.gliph.chat", a)
		var got struct {
			User struct{ ID string } `json:"user"`
		}
		json.Unmarshal(w.Body.Bytes(), &got)
		return got.User.ID
	}
	if a, b := mint(), mint(); a == "" || a != b {
		t.Fatalf("two sign-ins made two accounts: %q and %q", a, b)
	}
}

// The audience is what makes a shard safe to share. Without it an assertion for
// one team would be a session on its neighbour.
func TestAnAssertionForOneTeamIsWorthlessOnAnother(t *testing.T) {
	sh, priv := ssoShard(t)
	a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
		Aud: "acme", Email: "waseem@sadiq.nl", Exp: time.Now().Add(time.Minute).Unix(),
	})
	if w := redeem(sh, "hooli.gliph.chat", a); w.Code != 403 {
		t.Fatalf("got %d, want 403", w.Code)
	}
	if n := sh.tenantFor("hooli.gliph.chat").St.CountUsers(); n != 0 {
		t.Fatalf("hooli gained %d accounts from acme's assertion", n)
	}
}

func TestAnUnsignedOrStaleAssertionIsRefused(t *testing.T) {
	sh, priv := ssoShard(t)
	_, other, _ := ed25519.GenerateKey(nil)

	cases := map[string]string{
		"signed by somebody else": signAssertion(t, other, ssoClaims{Jti: id("j"), 
			Aud: "acme", Email: "e@x", Exp: time.Now().Add(time.Minute).Unix()}),
		"expired": signAssertion(t, priv, ssoClaims{Jti: id("j"), 
			Aud: "acme", Email: "e@x", Exp: time.Now().Add(-time.Second).Unix()}),
		"names nobody": signAssertion(t, priv, ssoClaims{Jti: id("j"), 
			Aud: "acme", Exp: time.Now().Add(time.Minute).Unix()}),
		"no audience": signAssertion(t, priv, ssoClaims{Jti: id("j"), 
			Email: "e@x", Exp: time.Now().Add(time.Minute).Unix()}),
		"not an assertion at all": "garbage",
		"body edited after signing": func() string {
			good := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
				Aud: "acme", Email: "e@x", Exp: time.Now().Add(time.Minute).Unix()})
			_, sig, _ := strings.Cut(good, ".")
			evil, _ := json.Marshal(ssoClaims{Jti: id("j"), Aud: "acme", Email: "admin@x",
				Exp: time.Now().Add(time.Minute).Unix()})
			return base64.RawURLEncoding.EncodeToString(evil) + "." + sig
		}(),
	}
	for name, a := range cases {
		if w := redeem(sh, "acme.gliph.chat", a); w.Code != 403 {
			t.Errorf("%s: got %d, want 403", name, w.Code)
		}
	}
	if n := sh.tenantFor("acme.gliph.chat").St.CountUsers(); n != 0 {
		t.Fatalf("a refused assertion still created %d accounts", n)
	}
}

// A self-hosted server has no control plane and must not grow a second front
// door because one exists elsewhere.
func TestAServerWithNoControlKeyRefusesSSOEntirely(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	_, priv, _ := ed25519.GenerateKey(nil)
	a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
		Aud: "acme", Email: "e@x", Exp: time.Now().Add(time.Minute).Unix()})
	if w := redeem(sh, "acme.gliph.chat", a); w.Code != 404 {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// Two people whose emails suggest the same nick both get in.
func TestASSONickClashGetsASuffixRatherThanAFailure(t *testing.T) {
	sh, priv := ssoShard(t)
	for _, email := range []string{"waseem@a.example", "waseem@b.example"} {
		a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
			Aud: "acme", Email: email, Nick: "waseem",
			Exp: time.Now().Add(time.Minute).Unix()})
		if w := redeem(sh, "acme.gliph.chat", a); w.Code != 200 {
			t.Fatalf("%s: %d %s", email, w.Code, w.Body)
		}
	}
	st := sh.tenantFor("acme.gliph.chat").St
	if n := st.CountUsers(); n != 2 {
		t.Fatalf("accounts = %d, want 2", n)
	}
	if _, err := st.UserByNick("waseem2"); err != nil {
		t.Fatalf("the second arrival did not get a free nick: %v", err)
	}
}

// The other half of control/control_test.go's TestTheWireFormatIsStable.
//
// The assertion format is defined in two modules that cannot import each other,
// so it is pinned from both sides by the same literal: a real assertion the
// control plane minted, verified here by the shard's own code. Change the shape
// on one side and this fails, which is the only thing standing between a format
// drift and a production sign-in that stops working.
func TestTheControlPlaneWireFormatIsStable(t *testing.T) {
	const (
		assertion = "eyJhdWQiOiJhY21lIiwiZW1haWwiOiJ3YXNlZW1Ac2FkaXEubmwiLCJuaWNrIjoid2FzZWVtIiwiZXhwIjoxODAwMDAwMDYwLCJqdGkiOiJqMSJ9" +
			".5EdB7rHKk3nlP_9hEYuHjqme1_9As6JOxLsvqhlEUbZLOcwfBc6SGbqAhv7LODzapkelFVPlbqkunOhwRK4-CA"
		pubB64 = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	)
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	// The assertion in the pin expires at a fixed instant, so the clock is
	// fixed too — this is about the format, not about time.
	at := time.Unix(1_800_000_000, 0)

	c, err := verifyAssertion(ed25519.PublicKey(pub), "acme", assertion, at)
	if err != nil {
		t.Fatalf("the shard can no longer read what the control plane mints: %v", err)
	}
	if c.Email != "waseem@sadiq.nl" || c.Nick != "waseem" || c.Aud != "acme" || c.Jti != "j1" {
		t.Fatalf("claims = %+v", c)
	}
}

// The control plane does not prove an email address, so an assertion never
// signs in to a PASSWORD account that happens to share it: otherwise whoever
// registered the team admin's address there would become that admin here.
func TestSSONeverTakesOverAPasswordAccount(t *testing.T) {
	sh, priv := ssoShard(t)
	acme := sh.tenantFor("acme.gliph.chat")
	admin, err := acme.St.Register("boss", "boss@acme.example", "hunter2hunter2", sh.savedInvite("acme"))
	if err != nil {
		t.Fatal(err)
	}
	a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
		Aud: "acme", Email: "boss@acme.example", Nick: "boss",
		Exp: time.Now().Add(time.Minute).Unix(),
	})
	if w := redeem(sh, "acme.gliph.chat", a); w.Code != 403 {
		t.Fatalf("got %d %s, want 403", w.Code, w.Body)
	}
	var n int
	acme.St.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, admin.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("the admin's account got %d sessions from an assertion", n)
	}
}

// The control plane's owner runs the team here too: their sign-in is admin.
func TestTheOwnerSignsInAsAdmin(t *testing.T) {
	sh, priv := ssoShard(t)
	a := signAssertion(t, priv, ssoClaims{Jti: id("j"), 
		Aud: "acme", Email: "owner@example.com", Nick: "owner", Role: "owner",
		Exp: time.Now().Add(time.Minute).Unix(),
	})
	w := redeem(sh, "acme.gliph.chat", a)
	var got struct {
		User struct{ Role string } `json:"user"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got.User.Role != "admin" {
		t.Fatalf("%d role=%q, want admin", w.Code, got.User.Role)
	}
}

// An assertion is single-use: a copy redeemed a second time mints nothing.
func TestAnAssertionWorksOnce(t *testing.T) {
	sh, priv := ssoShard(t)
	a := signAssertion(t, priv, ssoClaims{
		Aud: "acme", Email: "once@example.com", Nick: "once", Jti: "only-once",
		Exp: time.Now().Add(time.Minute).Unix(),
	})
	if w := redeem(sh, "acme.gliph.chat", a); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := redeem(sh, "acme.gliph.chat", a); w.Code != 403 {
		t.Fatalf("second: %d, want 403", w.Code)
	}
}
