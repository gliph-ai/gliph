package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("GLIPH_CONTROL_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://localhost/gliph_control_test?sslmode=disable"
	}
	st, err := OpenStore(dsn)
	if err != nil {
		t.Skipf("no test postgres (%v) — set GLIPH_CONTROL_TEST_DSN or `createdb gliph_control_test`", err)
	}
	// Every test starts from empty. CASCADE because memberships and sessions
	// hang off both sides.
	if _, err := st.db.Exec(
		`TRUNCATE memberships, account_sessions, servers, accounts, shards CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A shard that answers provisioning, so a create can be tested end to end
// without a real one.
func fakeShard(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(403)
			return
		}
		var d struct{ Slug string }
		json.NewDecoder(r.Body).Decode(&d)
		writeJSON(w, 200, map[string]any{"slug": d.Slug, "invite": "inv_" + d.Slug})
	}))
	t.Cleanup(s.Close)
	return s
}

func testAPI(t *testing.T, st *Store) (*API, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{St: st, Priv: priv, Domain: "gliph.chat", AdminSecret: "ops"}
	a.Mail = func(to, subject, text string) error {
		lastCode[to] = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(text, "\n", 2)[0], "Your code is "))
		return nil
	}
	return a, pub
}

// lastCode is the verification code most recently mailed to each address.
var lastCode = map[string]string{}

func call(a *API, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Routes().ServeHTTP(w, r)
	return w
}

func signupToken(t *testing.T, a *API, email string) string {
	t.Helper()
	if w := call(a, "POST", "/api/signup", "", `{"email":"`+email+`","password":"hunter2hunter2"}`); w.Code != 202 {
		t.Fatalf("signup %s: %d %s", email, w.Code, w.Body)
	}
	w := call(a, "POST", "/api/login", "", `{"email":"`+email+`","password":"hunter2hunter2"}`)
	var got struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &got)
	// Every test account proves its address the way a person does: with the
	// code from the mail.
	if v := call(a, "POST", "/api/verify", got.Token, `{"code":"`+lastCode[email]+`"}`); v.Code != 200 {
		t.Fatalf("verify %s: %d %s", email, v.Code, v.Body)
	}
	return got.Token
}

// unverifiedToken signs up and stops there.
func unverifiedToken(t *testing.T, a *API, email string) string {
	t.Helper()
	call(a, "POST", "/api/signup", "", `{"email":"`+email+`","password":"hunter2hunter2"}`)
	w := call(a, "POST", "/api/login", "", `{"email":"`+email+`","password":"hunter2hunter2"}`)
	var got struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &got)
	return got.Token
}

// ---- accounts ----

func TestSignupThenLogin(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)

	tok := signupToken(t, a, "waseem@sadiq.nl")
	acc, err := st.AccountByToken(tok)
	if err != nil || acc.Email != "waseem@sadiq.nl" {
		t.Fatalf("token did not resolve: %v %+v", err, acc)
	}

	// The address is not case sensitive, and neither is the login.
	if w := call(a, "POST", "/api/login", "", `{"email":"WASEEM@Sadiq.NL","password":"hunter2hunter2"}`); w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	if w := call(a, "POST", "/api/login", "", `{"email":"waseem@sadiq.nl","password":"wrong"}`); w.Code != 401 {
		t.Fatalf("a wrong password got %d", w.Code)
	}
}

func TestOneAccountPerEmail(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	signupToken(t, a, "waseem@sadiq.nl")
	// The same answer as a new address, so signup reveals nobody; and the
	// second password is not a way in.
	if w := call(a, "POST", "/api/signup", "", `{"email":"waseem@sadiq.nl","password":"other-password"}`); w.Code != 202 {
		t.Fatalf("a second signup got %d, want the same 202 as a first", w.Code)
	}
	if w := call(a, "POST", "/api/login", "", `{"email":"waseem@sadiq.nl","password":"other-password"}`); w.Code != 401 {
		t.Fatalf("the second signup's password signed in: %d", w.Code)
	}
}

// "No such account" and "wrong password" must read identically, or this is a
// free check for which addresses have signed up.
func TestLoginDoesNotSayWhichAccountsExist(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	signupToken(t, a, "real@example.com")
	missing := call(a, "POST", "/api/login", "", `{"email":"nobody@example.com","password":"x"}`)
	wrong := call(a, "POST", "/api/login", "", `{"email":"real@example.com","password":"x"}`)
	if missing.Code != wrong.Code || missing.Body.String() != wrong.Body.String() {
		t.Fatalf("the two answers differ:\n%d %s\n%d %s",
			missing.Code, missing.Body, wrong.Code, wrong.Body)
	}
}

// ---- placement ----

func TestPlacementPicksTheEmptiestShardWithRoom(t *testing.T) {
	st := testStore(t)
	for _, sh := range []Shard{
		{Name: "chuckles-1", Pool: "shared", BaseURL: "http://c1", Secret: "s", Capacity: 2, Accepting: true},
		{Name: "chuckles-2", Pool: "shared", BaseURL: "http://c2", Secret: "s", Capacity: 2, Accepting: true},
	} {
		if err := st.AddShard(sh); err != nil {
			t.Fatal(err)
		}
	}
	// Both empty: the tie breaks by name, so placement is deterministic.
	got, err := st.Place("shared")
	if err != nil || got.Name != "chuckles-1" {
		t.Fatalf("first placement: %v %+v", err, got)
	}
	st.CreateServer("acme", "acme", "acme.gliph.chat", "chuckles-1", "shared", "")
	if got, _ := st.Place("shared"); got.Name != "chuckles-2" {
		t.Fatalf("second placement went to %s, want the emptier chuckles-2", got.Name)
	}
}

func TestPlacementRefusesWhenEveryShardIsFull(t *testing.T) {
	st := testStore(t)
	st.AddShard(Shard{Name: "chuckles-1", Pool: "shared", BaseURL: "http://c1", Secret: "s", Capacity: 1, Accepting: true})
	st.CreateServer("acme", "acme", "acme.gliph.chat", "chuckles-1", "shared", "")
	if _, err := st.Place("shared"); err == nil {
		t.Fatal("placed a team on a full shard")
	}
	// A shard that is up but closed to new teams takes none.
	st.AddShard(Shard{Name: "chuckles-2", Pool: "shared", BaseURL: "http://c2", Secret: "s", Capacity: 9, Accepting: false})
	if _, err := st.Place("shared"); err == nil {
		t.Fatal("placed a team on a shard that is not accepting")
	}
}

// A dedicated customer is a pool of one, which needs no special case anywhere.
func TestADedicatedPoolIsJustAPool(t *testing.T) {
	st := testStore(t)
	st.AddShard(Shard{Name: "shared-1", Pool: "shared", BaseURL: "http://s1", Secret: "s", Capacity: 50, Accepting: true})
	st.AddShard(Shard{Name: "acme-1", Pool: "acme", BaseURL: "http://a1", Secret: "s", Capacity: 1, Accepting: true})
	got, err := st.Place("acme")
	if err != nil || got.Name != "acme-1" {
		t.Fatalf("%v %+v", err, got)
	}
}

// ---- creating a team ----

func TestCreatingATeamPlacesProvisionsAndMakesYouOwner(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s3cret")
	st.AddShard(Shard{Name: "chuckles-1", Pool: "shared", BaseURL: shard.URL, Secret: "s3cret", Capacity: 5, Accepting: true})

	tok := signupToken(t, a, "waseem@sadiq.nl")
	w := call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme","nick":"waseem"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Server Server `json:"server"`
		Invite string `json:"invite"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	// The customer's address names the TEAM, never the shard: name it after the
	// machine and the team can never move.
	if got.Server.Host != "acme.gliph.chat" {
		t.Errorf("host = %q", got.Server.Host)
	}
	// One way in for a hosted team: single sign-on. The shard's password
	// invite is not handed out.
	if got.Invite != "" {
		t.Errorf("the owner was handed a password invite: %q", got.Invite)
	}

	list := call(a, "GET", "/api/servers", tok, "")
	if !strings.Contains(list.Body.String(), `"role":"owner"`) {
		t.Errorf("the creator is not the owner: %s", list.Body)
	}
}

func TestTwoTeamsCannotShareAnAddress(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})

	one := signupToken(t, a, "one@example.com")
	two := signupToken(t, a, "two@example.com")
	if w := call(a, "POST", "/api/servers", one, `{"slug":"acme","name":"Acme"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := call(a, "POST", "/api/servers", two, `{"slug":"acme","name":"Also Acme"}`); w.Code != 409 {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestAnAddressTheShardWouldRefuseIsRefusedHere(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	tok := signupToken(t, a, "waseem@sadiq.nl")
	for _, slug := range []string{"", "Acme", "a b", "../etc"} {
		body, _ := json.Marshal(map[string]string{"slug": slug, "name": "x"})
		if w := call(a, "POST", "/api/servers", tok, string(body)); w.Code != 400 {
			t.Errorf("slug %q got %d, want 400", slug, w.Code)
		}
	}
}

// ---- the way in ----

func TestSSOIsOnlyMintedForATeamYouAreIn(t *testing.T) {
	st := testStore(t)
	a, pub := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})

	owner := signupToken(t, a, "owner@example.com")
	call(a, "POST", "/api/servers", owner, `{"slug":"acme","name":"Acme","nick":"waseem"}`)
	stranger := signupToken(t, a, "stranger@example.com")

	w := call(a, "POST", "/api/servers/acme/sso", owner, "")
	if w.Code != 200 {
		t.Fatalf("owner: %d %s", w.Code, w.Body)
	}
	var got struct{ Assertion, Host string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Host != "https://acme.gliph.chat" {
		t.Errorf("host = %q", got.Host)
	}
	// The assertion names the team and the person, and is signed.
	body, sig, _ := strings.Cut(got.Assertion, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	sigBytes, _ := base64.RawURLEncoding.DecodeString(sig)
	if !ed25519.Verify(pub, raw, sigBytes) {
		t.Fatal("the assertion does not verify against the public half")
	}
	var c claims
	json.Unmarshal(raw, &c)
	if c.Aud != "acme" || c.Email != "owner@example.com" || c.Nick != "waseem" {
		t.Fatalf("claims = %+v", c)
	}
	if c.Exp <= time.Now().Unix() || c.Exp > time.Now().Add(2*time.Minute).Unix() {
		t.Fatalf("exp is not a short life: %d", c.Exp)
	}

	if w := call(a, "POST", "/api/servers/acme/sso", stranger, ""); w.Code != 403 {
		t.Fatalf("a stranger got %d, want 403", w.Code)
	}
}

// Billing lives in the control plane and nowhere else: the shard stays dumb
// about money, and this is how that is enforced.
func TestASuspendedTeamCannotBeSignedInTo(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})
	tok := signupToken(t, a, "waseem@sadiq.nl")
	call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme"}`)

	// past_due still lets people in: locking a team out of its own history over
	// a failed card is a thing to threaten, not to do.
	st.SetStatus("acme", StatusPastDue, "inv_123")
	if w := call(a, "POST", "/api/servers/acme/sso", tok, ""); w.Code != 200 {
		t.Fatalf("past_due got %d, want 200", w.Code)
	}
	st.SetStatus("acme", StatusSuspended, "inv_123")
	if w := call(a, "POST", "/api/servers/acme/sso", tok, ""); w.Code != 402 {
		t.Fatalf("suspended got %d, want 402", w.Code)
	}
}

func TestOnlyAnOwnerAddsPeople(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})

	owner := signupToken(t, a, "owner@example.com")
	call(a, "POST", "/api/servers", owner, `{"slug":"acme","name":"Acme"}`)
	member := signupToken(t, a, "member@example.com")

	if w := call(a, "POST", "/api/servers/acme/members", owner,
		`{"email":"member@example.com","nick":"robin"}`); w.Code != 200 {
		t.Fatalf("owner adding: %d %s", w.Code, w.Body)
	}
	// The team now shows up in their own list, which is the whole point of the
	// directory.
	if l := call(a, "GET", "/api/servers", member, ""); !strings.Contains(l.Body.String(), "acme") {
		t.Fatalf("the new member cannot see the team: %s", l.Body)
	}
	// A member is not an owner.
	if w := call(a, "POST", "/api/servers/acme/members", member,
		`{"email":"owner@example.com"}`); w.Code != 403 {
		t.Fatalf("a member adding somebody got %d, want 403", w.Code)
	}
	// Nor is somebody with no membership at all, and the answer is the same so
	// this does not report which teams exist.
	stranger := signupToken(t, a, "stranger@example.com")
	if w := call(a, "POST", "/api/servers/acme/members", stranger,
		`{"email":"owner@example.com"}`); w.Code != 403 {
		t.Fatalf("a stranger got %d, want 403", w.Code)
	}
}

func TestTheShardRegistryIsOperationsOnly(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	customer := signupToken(t, a, "waseem@sadiq.nl")

	body := `{"name":"c1","base_url":"http://c1","secret":"s","capacity":5,"accepting":true}`
	if w := call(a, "POST", "/api/shards", customer, body); w.Code != 403 {
		t.Fatalf("a customer registering a shard got %d, want 403", w.Code)
	}
	if w := call(a, "POST", "/api/shards", "ops", body); w.Code != 200 {
		t.Fatalf("operations: %d %s", w.Code, w.Body)
	}
	if w := call(a, "GET", "/api/shards", "ops", ""); !strings.Contains(w.Body.String(), "c1") {
		t.Fatalf("listing: %s", w.Body)
	}
}

// The assertion format is defined in two modules that cannot import each other,
// so it is pinned from both sides by the same literal. The twin of this test is
// server/sso_test.go's TestTheControlPlaneWireFormatIsStable — change one and
// the other fails, which is the point.
func TestTheWireFormatIsStable(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	at := time.Unix(1_800_000_000, 0)

	got, err := mintAssertion(priv, "acme", "waseem@sadiq.nl", "waseem", "", "j1", at)
	if err != nil {
		t.Fatal(err)
	}
	const want = "eyJhdWQiOiJhY21lIiwiZW1haWwiOiJ3YXNlZW1Ac2FkaXEubmwiLCJuaWNrIjoid2FzZWVtIiwiZXhwIjoxODAwMDAwMDYwLCJqdGkiOiJqMSJ9"
	body, _, _ := strings.Cut(got, ".")
	if body != want {
		t.Fatalf("the assertion body changed shape.\n got %s\nwant %s\n"+
			"If this is deliberate, update server/sso_test.go in the same commit.", body, want)
	}
	t.Logf("pin for the shard side:\n%s\npublic key: %s",
		got, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
}

// A creation that failed after its row landed (the shard was down) is finished
// by the same caller retrying, not answered "taken" for ever. Anyone else still
// gets 409.
func TestAFailedCreationCanBeRetriedByItsCreator(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	down := true
	shard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, map[string]any{"slug": "acme", "invite": "inv_acme"})
	}))
	t.Cleanup(shard.Close)
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})

	me := signupToken(t, a, "me@example.com")
	other := signupToken(t, a, "other@example.com")
	if w := call(a, "POST", "/api/servers", me, `{"slug":"acme","name":"Acme"}`); w.Code != 502 {
		t.Fatalf("shard down: got %d, want 502", w.Code)
	}
	down = false
	if w := call(a, "POST", "/api/servers", other, `{"slug":"acme","name":"Acme"}`); w.Code != 409 {
		t.Fatalf("someone else: got %d, want 409", w.Code)
	}
	if w := call(a, "POST", "/api/servers", me, `{"slug":"acme","name":"Acme"}`); w.Code != 200 {
		t.Fatalf("the retry: %d %s", w.Code, w.Body)
	}
	if list := call(a, "GET", "/api/servers", me, ""); !strings.Contains(list.Body.String(), `"role":"owner"`) {
		t.Fatalf("the retry did not make me owner: %s", list.Body)
	}
}

// Suspension reaches the shard, so a session minted before it stops working
// too; lifting it reaches the shard the same way.
func TestSuspensionIsPushedToTheShard(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	var pushed []string
	shard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			var d struct{ Suspended bool }
			json.NewDecoder(r.Body).Decode(&d)
			pushed = append(pushed, fmt.Sprintf("%s=%v", r.URL.Path, d.Suspended))
		}
		writeJSON(w, 200, map[string]any{"slug": "acme", "invite": "inv_acme"})
	}))
	t.Cleanup(shard.Close)
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme"}`)

	if w := call(a, "POST", "/api/servers/acme/status", "ops", `{"status":"suspended"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	call(a, "POST", "/api/servers/acme/status", "ops", `{"status":"active"}`)
	want := []string{"/shard/tenants/acme/status=true", "/shard/tenants/acme/status=false"}
	if fmt.Sprint(pushed) != fmt.Sprint(want) {
		t.Fatalf("pushed %v, want %v", pushed, want)
	}
}

// An address nobody has proved cannot be handed a team. Otherwise registering
// someone else's email first would collect every invitation meant for them.
func TestAnUnverifiedAccountGetsNoTeam(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})

	squatter := unverifiedToken(t, a, "victim@example.com")
	if w := call(a, "POST", "/api/servers", squatter, `{"slug":"mine","name":"Mine"}`); w.Code != 403 {
		t.Fatalf("create while unverified: %d, want 403", w.Code)
	}
	owner := signupToken(t, a, "owner@example.com")
	call(a, "POST", "/api/servers", owner, `{"slug":"acme","name":"Acme"}`)
	if w := call(a, "POST", "/api/servers/acme/members", owner, `{"email":"victim@example.com"}`); w.Code != 404 {
		t.Fatalf("adding an unverified address: %d, want 404", w.Code)
	}
	if w := call(a, "POST", "/api/verify", squatter, `{"code":"wrong"}`); w.Code != 400 {
		t.Fatalf("a wrong code: %d, want 400", w.Code)
	}
}

// A customer cannot spend a dedicated pool's capacity by naming it.
func TestACustomerCannotPickAPool(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shared, dedicated := fakeShard(t, "s"), fakeShard(t, "d")
	st.AddShard(Shard{Name: "shared-1", Pool: "shared", BaseURL: shared.URL, Secret: "s", Capacity: 9, Accepting: true})
	st.AddShard(Shard{Name: "bigco-1", Pool: "bigco", BaseURL: dedicated.URL, Secret: "d", Capacity: 1, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	call(a, "POST", "/api/servers", tok, `{"slug":"sneaky","name":"x","pool":"bigco"}`)
	if sh, err := st.ShardFor("sneaky"); err != nil || sh.Name != "shared-1" {
		t.Fatalf("placed on %v %v, want shared-1", sh, err)
	}
}

// The creator's assertion says owner, so the shard can make them its admin.
func TestTheOwnersAssertionSaysOwner(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme","nick":"me"}`)
	w := call(a, "POST", "/api/servers/acme/sso", tok, "")
	var got struct{ Assertion string }
	json.Unmarshal(w.Body.Bytes(), &got)
	body, _, _ := strings.Cut(got.Assertion, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	var c claims
	json.Unmarshal(raw, &c)
	if c.Role != "owner" {
		t.Fatalf("role = %q, want owner", c.Role)
	}
}

// A suspension the shard did not hear about is pushed again until it lands.
func TestAFailedStatusPushIsRetried(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	up := false
	var got []bool
	shard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			if !up {
				w.WriteHeader(503)
				return
			}
			var d struct{ Suspended bool }
			json.NewDecoder(r.Body).Decode(&d)
			got = append(got, d.Suspended)
		}
		writeJSON(w, 200, map[string]any{"slug": "acme", "invite": "i"})
	}))
	t.Cleanup(shard.Close)
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme"}`)

	if w := call(a, "POST", "/api/servers/acme/status", "ops", `{"status":"suspended"}`); w.Code != 202 {
		t.Fatalf("shard down: %d, want 202", w.Code)
	}
	up = true
	a.Reconcile()
	if len(got) != 1 || !got[0] {
		t.Fatalf("the retry pushed %v", got)
	}
	a.Reconcile() // synced now: nothing more to push
	if len(got) != 1 {
		t.Fatalf("pushed again after it landed: %v", got)
	}
}

// Login and resend have budgets: a script cannot turn them into free bcrypt
// work or a mail cannon.
func TestLoginAndResendAreLimited(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	tok := signupToken(t, a, "me@example.com")
	last := 0
	for i := 0; i < 12; i++ {
		last = call(a, "POST", "/api/login", "", `{"email":"me@example.com","password":"wrong"}`).Code
	}
	if last != 429 {
		t.Fatalf("the 12th login try got %d, want 429", last)
	}
	call(a, "POST", "/api/verify/resend", tok, "")
	if w := call(a, "POST", "/api/verify/resend", tok, ""); w.Code != 429 {
		t.Fatalf("a second resend within the minute got %d, want 429", w.Code)
	}
}

// Registering someone else's address reserves nothing: until a code from the
// mailbox confirms it, the next signup with that address takes it over.
func TestTheMailboxOwnerReclaimsAnUnconfirmedAccount(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	squatter := unverifiedToken(t, a, "victim@example.com")
	if w := call(a, "POST", "/api/signup", "", `{"email":"victim@example.com","password":"victims-own-pw"}`); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, err := st.AccountByToken(squatter); err == nil {
		t.Fatal("the squatter's session survived the reclaim")
	}
	w := call(a, "POST", "/api/login", "", `{"email":"victim@example.com","password":"victims-own-pw"}`)
	var got struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if v := call(a, "POST", "/api/verify", got.Token, `{"code":"`+lastCode["victim@example.com"]+`"}`); v.Code != 200 {
		t.Fatalf("the owner could not confirm: %d %s", v.Code, v.Body)
	}
	// confirmed now: another signup changes nothing
	call(a, "POST", "/api/signup", "", `{"email":"victim@example.com","password":"squatter-again"}`)
	if w := call(a, "POST", "/api/login", "", `{"email":"victim@example.com","password":"victims-own-pw"}`); w.Code != 200 {
		t.Fatalf("a signup took a confirmed account: %d", w.Code)
	}
}

// A taken address is a 409 even when the pool has no room.
func TestATakenAddressIs409EvenWhenFull(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 1, Accepting: true})
	one := signupToken(t, a, "one@example.com")
	two := signupToken(t, a, "two@example.com")
	call(a, "POST", "/api/servers", one, `{"slug":"acme","name":"Acme"}`)
	if w := call(a, "POST", "/api/servers", two, `{"slug":"acme","name":"Acme"}`); w.Code != 409 {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

// Shared capacity is everyone's: one account starts a bounded number of teams.
func TestAnAccountStartsABoundedNumberOfTeams(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 99, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	for i := 0; i < maxTeamsPerAccount; i++ {
		if w := call(a, "POST", "/api/servers", tok, fmt.Sprintf(`{"slug":"t%d","name":"x"}`, i)); w.Code != 200 {
			t.Fatalf("team %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := call(a, "POST", "/api/servers", tok, `{"slug":"one-more","name":"x"}`); w.Code != 403 {
		t.Fatalf("past the ceiling: %d, want 403", w.Code)
	}
}

// A login that read the old password before a takeover gets a session that is
// dead on arrival: it carries the old generation.
func TestASessionFromBeforeATakeoverIsDead(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	call(a, "POST", "/api/signup", "", `{"email":"x@example.com","password":"first-password"}`)
	var id string
	var gen int64
	st.db.QueryRow(`SELECT id,session_gen FROM accounts WHERE email='x@example.com'`).Scan(&id, &gen)
	call(a, "POST", "/api/signup", "", `{"email":"x@example.com","password":"second-password"}`) // the takeover
	// the slow login, landing now with what it read before
	st.db.Exec(`INSERT INTO account_sessions(token,account_id,gen) VALUES('late',$1,$2)`, id, gen)
	if _, err := st.AccountByToken("late"); err == nil {
		t.Fatal("a session from before the takeover works")
	}
}

func TestSlugsAreHostnameLabels(t *testing.T) {
	for s, want := range map[string]bool{"acme": true, "a-b": true, "-": false, "-acme": false, "acme-": false, "a": true} {
		if validSlug(s) != want {
			t.Errorf("validSlug(%q) = %v", s, !want)
		}
	}
}

// A creation that finished but lost its answer: the creator's retry gets the
// team back, not 409.
func TestACompletedCreationRetryReturnsTheTeam(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	shard := fakeShard(t, "s")
	st.AddShard(Shard{Name: "c1", Pool: "shared", BaseURL: shard.URL, Secret: "s", Capacity: 9, Accepting: true})
	tok := signupToken(t, a, "me@example.com")
	call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme"}`)
	if w := call(a, "POST", "/api/servers", tok, `{"slug":"acme","name":"Acme"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"slug":"acme"`) {
		t.Fatalf("the creator's retry: %d %s", w.Code, w.Body)
	}
}

// A shard that records release and adopt, and can be told to refuse adopt.
func moveShard(t *testing.T, refuse bool, log *[]string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*log = append(*log, r.Host+" "+r.URL.Path)
		if refuse && strings.HasSuffix(r.URL.Path, "/adopt") {
			w.WriteHeader(500)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	t.Cleanup(s.Close)
	return s
}

// One call drains the team on its shard, resumes it on the target and
// repoints the directory; a target that cannot take it leaves it where it was.
func TestMovingATeamIsOneCommand(t *testing.T) {
	st := testStore(t)
	a, _ := testAPI(t, st)
	var log []string
	sa, sb, sc := moveShard(t, false, &log), moveShard(t, false, &log), moveShard(t, true, &log)
	for name, s := range map[string]*httptest.Server{"a": sa, "b": sb, "c": sc} {
		st.AddShard(Shard{Name: name, Pool: "shared", BaseURL: s.URL, Secret: "s", Capacity: 5, Accepting: true})
	}
	if _, err := st.CreateServer("acme", "Acme", "acme.gliph.chat", "a", "shared", ""); err != nil {
		t.Fatal(err)
	}
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

	if w := call(a, "POST", "/api/servers/acme/move", "", `{"shard":"b"}`); w.Code != 403 {
		t.Fatalf("a move without the admin secret: %d", w.Code)
	}
	if w := call(a, "POST", "/api/servers/acme/move", "ops", `{"shard":"b"}`); w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body)
	}
	want := []string{host(sa) + " /shard/tenants/acme/release", host(sb) + " /shard/tenants/acme/adopt"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", log, want)
	}
	if sh, _ := st.ShardFor("acme"); sh.Name != "b" {
		t.Fatalf("directory names %s", sh.Name)
	}

	log = nil
	if w := call(a, "POST", "/api/servers/acme/move", "ops", `{"shard":"c"}`); w.Code != 502 {
		t.Fatalf("a move to a refusing shard: %d", w.Code)
	}
	want = []string{host(sb) + " /shard/tenants/acme/release", host(sc) + " /shard/tenants/acme/adopt",
		host(sb) + " /shard/tenants/acme/adopt"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", log, want)
	}
	if sh, _ := st.ShardFor("acme"); sh.Name != "b" {
		t.Fatalf("a failed move repointed the team to %s", sh.Name)
	}
}
