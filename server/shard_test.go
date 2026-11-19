package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"fmt"
	"os"
	"sync"
	"strings"
	"testing"
	"time"
)

// shardPG is a fresh PostgreSQL database for one shard test. Sharding is
// hosted-only and runs on PostgreSQL, so without GLIPH_TEST_PG these skip.
func shardPG(t *testing.T) string {
	t.Helper()
	if os.Getenv("GLIPH_TEST_PG") == "" {
		t.Skip("sharding needs PostgreSQL: set GLIPH_TEST_PG")
	}
	return testDSN(t)
}

func testShard(t *testing.T) *Shard {
	t.Helper()
	sh := NewShard(ShardOpts{
		Version: "test", Started: time.Now(),
		Senders: byPlatform{}, Secret: "shhh", PG: shardPG(t), DataDir: t.TempDir(),
	})
	t.Cleanup(sh.Close)
	return sh
}

func provision(t *testing.T, sh *Shard, secret, slug, host string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"slug":"` + slug + `","name":"` + slug + `","hosts":["` + host + `"]}`
	r := httptest.NewRequest("POST", "/shard/tenants", strings.NewReader(body))
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, r)
	return w
}

// The whole point of the design: tenancy is resolved once, at the edge.
func TestAShardRoutesByHost(t *testing.T) {
	sh := testShard(t)
	if w := provision(t, sh, "shhh", "acme", "acme.gliph.chat"); w.Code != 200 {
		t.Fatalf("provision acme: %d %s", w.Code, w.Body)
	}
	if w := provision(t, sh, "shhh", "hooli", "hooli.gliph.chat"); w.Code != 200 {
		t.Fatalf("provision hooli: %d %s", w.Code, w.Body)
	}

	for host, want := range map[string]string{
		"acme.gliph.chat":      "acme",
		"hooli.gliph.chat":     "hooli",
		"ACME.Gliph.Chat":      "acme", // capitals are not identity
		"acme.gliph.chat:8787": "acme", // nor is a port
		"acme.gliph.chat.":     "acme", // nor is a trailing root dot
	} {
		got := sh.tenantFor(host)
		if got == nil || got.Slug != want {
			t.Errorf("%s resolved to %v, want %s", host, got, want)
		}
	}
}

// Wildcard DNS points every name on the domain at this process, so an unknown
// Host must never bring a database into existence.
func TestAnUnknownHostIsRefusedAndCreatesNothing(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")

	w := httptest.NewRecorder()
	sh.ServeHTTP(w, httptest.NewRequest("GET", "/api/me", nil).WithContext(t.Context()))
	if w.Code != 404 {
		t.Fatalf("a request with no known Host: %d, want 404", w.Code)
	}
	if n := len(sh.Tenants()); n != 1 {
		t.Fatalf("tenants = %d, want 1 — an unknown Host created one", n)
	}
}

func TestProvisioningNeedsTheSecret(t *testing.T) {
	sh := testShard(t)
	for _, secret := range []string{"", "wrong", "shh"} {
		if w := provision(t, sh, secret, "acme", "acme.gliph.chat"); w.Code != 403 {
			t.Errorf("secret %q got %d, want 403", secret, w.Code)
		}
	}
	if n := len(sh.Tenants()); n != 0 {
		t.Fatalf("tenants = %d, want 0", n)
	}
}

// A shard with no secret at all is a self-hosted server: it hosts what it hosts
// and nothing can add to it.
func TestAShardWithNoSecretAcceptsNoProvisioning(t *testing.T) {
	sh := NewShard(ShardOpts{Senders: byPlatform{}, PG: shardPG(t), DataDir: t.TempDir()})
	t.Cleanup(sh.Close)
	if w := provision(t, sh, "", "acme", "acme.gliph.chat"); w.Code != 403 {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// The control plane retries. A retry that made a second team would be worse
// than one that failed.
func TestProvisioningIsIdempotent(t *testing.T) {
	sh := testShard(t)
	first := provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	var got struct {
		Invite   string `json:"invite"`
		Existing bool   `json:"existing"`
	}
	json.Unmarshal(first.Body.Bytes(), &got)
	if got.Invite == "" {
		t.Fatal("a fresh team comes with the invite that makes its first account admin")
	}

	again := provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	if again.Code != 200 {
		t.Fatalf("second provision: %d", again.Code)
	}
	json.Unmarshal(again.Body.Bytes(), &got)
	if !got.Existing {
		t.Error("the second call should report the team it found, not make another")
	}
	if n := len(sh.Tenants()); n != 1 {
		t.Fatalf("tenants = %d, want 1", n)
	}
}

func TestTwoTeamsCannotClaimOneHost(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	if w := provision(t, sh, "shhh", "impostor", "acme.gliph.chat"); w.Code != 409 {
		t.Fatalf("got %d, want a refusal", w.Code)
	}
	if got := sh.tenantFor("acme.gliph.chat"); got == nil || got.Slug != "acme" {
		t.Fatal("the host must still belong to the team that had it")
	}
}

func TestABadSlugIsRefused(t *testing.T) {
	sh := testShard(t)
	for _, slug := range []string{"", "../etc", "Acme", "a b", strings.Repeat("x", 41)} {
		if w := provision(t, sh, "shhh", slug, "x.gliph.chat"); w.Code != 400 {
			t.Errorf("slug %q got %d, want 400", slug, w.Code)
		}
	}
}

// A self-hosted server answers whatever Host arrives, because it is one team
// and there is nothing to route between.
func TestSoloServesEveryHost(t *testing.T) {
	sh := NewShard(ShardOpts{Senders: byPlatform{}, DataDir: t.TempDir()})
	t.Cleanup(sh.Close)
	tn, err := sh.Open("", "gliph.local", nil)
	if err != nil {
		t.Fatal(err)
	}
	sh.Solo(tn)
	for _, host := range []string{"anything.example", "localhost:8787", ""} {
		if got := sh.tenantFor(host); got != tn {
			t.Errorf("%q did not reach the one team", host)
		}
	}
}

// The claim the whole design rests on: a token minted by one team is not a
// credential on another. Nothing below the Host lookup knows tenancy exists,
// so this has to be true by construction — and asserted, because "by
// construction" is what everyone says before the leak.
func TestATokenFromOneTeamIsWorthlessOnAnother(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	provision(t, sh, "shhh", "hooli", "hooli.gliph.chat")

	acme := sh.tenantFor("acme.gliph.chat")
	if _, err := acme.St.Register("waseem", "w@example.com", "hunter2hunter2", sh.savedInvite("acme")); err != nil {
		t.Fatal(err)
	}
	tok, _, err := acme.St.Login("waseem", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}

	ask := func(host string) int {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		sh.ServeHTTP(w, r)
		return w.Code
	}
	if got := ask("acme.gliph.chat"); got != 200 {
		t.Fatalf("on its own server the token is good: got %d", got)
	}
	if got := ask("hooli.gliph.chat"); got != 401 {
		t.Fatalf("on another team's server it must be nothing: got %d, want 401", got)
	}
}

// A shard that restarts serves the teams it was serving, with no control plane
// in the loop.
func TestAShardComesBackUpKnowingItsTeams(t *testing.T) {
	pg, dir := shardPG(t), t.TempDir()
	first := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: dir})
	provision(t, first, "shhh", "acme", "acme.gliph.chat")
	provision(t, first, "shhh", "hooli", "hooli.gliph.chat")
	first.Close()

	second := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: dir})
	t.Cleanup(second.Close)
	if err := second.Load(); err != nil {
		t.Fatal(err)
	}
	if n := len(second.Tenants()); n != 2 {
		t.Fatalf("after a restart: %d teams, want 2", n)
	}
	if got := second.tenantFor("hooli.gliph.chat"); got == nil || got.Slug != "hooli" {
		t.Fatal("hooli did not come back")
	}
}

func TestListingTeamsNeedsTheSecret(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")

	r := httptest.NewRequest("GET", "/shard/tenants", nil)
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("unauthenticated listing: %d, want 403", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/shard/tenants", nil)
	r.Header.Set("Authorization", "Bearer shhh")
	w = httptest.NewRecorder()
	sh.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "acme") {
		t.Fatalf("listing: %d %s", w.Code, w.Body)
	}
}

// Each team is its own schema: an account made on one is not a row the other
// can see, whatever query runs, because the other's search_path never reaches it.
func TestTeamsAreSeparateSchemas(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	provision(t, sh, "shhh", "hooli-2", "hooli.gliph.chat")
	acme, hooli := sh.tenantFor("acme.gliph.chat"), sh.tenantFor("hooli.gliph.chat")
	if acme.Home != "t_acme" || hooli.Home != "t_hooli_2" {
		t.Fatalf("schemas %q and %q", acme.Home, hooli.Home)
	}
	if _, err := acme.St.Register("waseem", "w@example.com", "hunter2hunter2", sh.savedInvite("acme")); err != nil {
		t.Fatal(err)
	}
	if n := hooli.St.CountUsers(); n != 0 {
		t.Fatalf("hooli sees %d of acme's accounts", n)
	}
	// and both still got their own #general
	if _, err := hooli.St.ChannelByName("general"); err != nil {
		t.Fatal(err)
	}
}

// Sharding is hosted-only: without PostgreSQL there is nowhere to put a team.
func TestASelfHostCannotProvisionTeams(t *testing.T) {
	sh := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", DataDir: t.TempDir()})
	t.Cleanup(sh.Close)
	if w := provision(t, sh, "shhh", "acme", "acme.gliph.chat"); w.Code != 500 {
		t.Fatalf("got %d, want a refusal", w.Code)
	}
}

// A suspended team refuses every request, so a session from before the
// suspension is worth nothing during it; lifting it brings the team back, and
// a restart remembers which it is.
func TestASuspendedTeamRefusesEverySession(t *testing.T) {
	pg, dir := shardPG(t), t.TempDir()
	sh := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: dir})
	t.Cleanup(sh.Close)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	acme := sh.tenantFor("acme.gliph.chat")
	acme.St.Register("waseem", "w@example.com", "hunter2hunter2", sh.savedInvite("acme"))
	tok, _, _ := acme.St.Login("waseem", "hunter2hunter2")

	me := func(s *Shard) int {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Host = "acme.gliph.chat"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	version := 0
	status := func(suspended bool) int {
		version++
		body := fmt.Sprintf(`{"suspended":%v,"version":%d}`, suspended, version)
		r := httptest.NewRequest("POST", "/shard/tenants/acme/status", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer shhh")
		w := httptest.NewRecorder()
		sh.ServeHTTP(w, r)
		return w.Code
	}
	if status(true) != 200 || me(sh) != 402 {
		t.Fatal("a suspended team still answered an old session")
	}
	again := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: dir})
	t.Cleanup(again.Close)
	if err := again.Load(); err != nil || me(again) != 402 {
		t.Fatal("a restart forgot the suspension")
	}
	if status(false) != 200 || me(sh) != 200 {
		t.Fatal("lifting the suspension did not bring the team back")
	}
}

// A team refused for a host already taken leaves no record behind.
func TestARefusedTeamLeavesNoRecord(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	if w := provision(t, sh, "shhh", "copycat", "acme.gliph.chat"); w.Code != 409 {
		t.Fatalf("got %d, want 409", w.Code)
	}
	tfs, err := sh.savedTenants()
	if err != nil {
		t.Fatal(err)
	}
	for _, tf := range tfs {
		if tf.Slug == "copycat" {
			t.Fatal("the refused team is in the registry")
		}
	}
}

// A retry after a lost answer gets the same owner invite, not an empty one.
func TestAProvisioningRetryReturnsTheInvite(t *testing.T) {
	sh := testShard(t)
	var first, again struct{ Invite string }
	json.Unmarshal(provision(t, sh, "shhh", "acme", "acme.gliph.chat").Body.Bytes(), &first)
	json.Unmarshal(provision(t, sh, "shhh", "acme", "acme.gliph.chat").Body.Bytes(), &again)
	if first.Invite == "" || again.Invite != first.Invite {
		t.Fatalf("first %q, retry %q", first.Invite, again.Invite)
	}
}

// Anyone can reach a new hosted team's address, so its first account needs the
// owner invite like every other: "first to register is admin" would hand the
// team to whoever got there before its owner.
func TestAHostedTeamsFirstAccountNeedsTheInvite(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	acme := sh.tenantFor("acme.gliph.chat")
	if _, err := acme.St.Register("early", "early@example.com", "hunter2hunter2", ""); err == nil {
		t.Fatal("a hosted team let its first account in without an invite")
	}
	u, err := acme.St.Register("owner", "owner@example.com", "hunter2hunter2", sh.savedInvite("acme"))
	if err != nil || u.Role != "admin" {
		t.Fatalf("the owner invite: %v %+v", err, u)
	}
}

// A provisioning that stopped before saving its invite gets one on the retry,
// as long as nobody is in the team yet.
func TestARetryMintsAMissingInvite(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	reg, _ := sh.registry()
	reg.Exec(`UPDATE gliph_tenants SET invite='' WHERE slug='acme'`)
	var again struct{ Invite string }
	json.Unmarshal(provision(t, sh, "shhh", "acme", "acme.gliph.chat").Body.Bytes(), &again)
	if again.Invite == "" {
		t.Fatal("the retry came back without an invite")
	}
}

// Shards share one registry. Each loads only its own teams, and an address
// one shard holds cannot be claimed by another.
func TestShardsShareARegistryWithoutSharingTeams(t *testing.T) {
	pg := shardPG(t)
	a := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: t.TempDir(), Name: "a"})
	b := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: t.TempDir(), Name: "b"})
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	provision(t, a, "shhh", "acme", "acme.gliph.chat")
	if w := provision(t, b, "shhh", "hooli", "acme.gliph.chat"); w.Code != 409 {
		t.Fatalf("shard b claimed shard a's address: %d", w.Code)
	}
	if w := provision(t, b, "shhh", "acme", "other.gliph.chat"); w.Code != 409 {
		t.Fatalf("shard b claimed shard a's slug: %d", w.Code)
	}
	again := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: t.TempDir(), Name: "b"})
	t.Cleanup(again.Close)
	if err := again.Load(); err != nil || len(again.Tenants()) != 0 {
		t.Fatalf("shard b loaded %d of shard a's teams (%v)", len(again.Tenants()), err)
	}
}

// Two provisioning requests for one team at once make one team.
func TestConcurrentProvisioningMakesOneTeam(t *testing.T) {
	sh := testShard(t)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = provision(t, sh, "shhh", "acme", "acme.gliph.chat").Code
		}(i)
	}
	wg.Wait()
	if n := len(sh.Tenants()); n != 1 {
		t.Fatalf("%d teams from one slug (codes %v)", n, codes)
	}
}

// A status push older than the one applied is ignored: two pushes that cross
// on the way cannot leave the team on the stale one.
func TestAStaleStatusPushIsIgnored(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	push := func(body string) {
		r := httptest.NewRequest("POST", "/shard/tenants/acme/status", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer shhh")
		sh.ServeHTTP(httptest.NewRecorder(), r)
	}
	push(`{"suspended":false,"version":2}`) // the newer one lands first
	push(`{"suspended":true,"version":1}`)  // the older one arrives late
	if sh.tenantFor("acme.gliph.chat").suspended.Load() {
		t.Fatal("the late, older push won")
	}
}

// A retry for a slug with other addresses is a different request, not a retry.
func TestARetryWithOtherHostsIsRefused(t *testing.T) {
	sh := testShard(t)
	provision(t, sh, "shhh", "acme", "acme.gliph.chat")
	if w := provision(t, sh, "shhh", "acme", "elsewhere.gliph.chat"); w.Code != 409 {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func shardCall(t *testing.T, sh *Shard, path string) int {
	t.Helper()
	r := httptest.NewRequest("POST", path, nil)
	r.Header.Set("Authorization", "Bearer shhh")
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, r)
	return w.Code
}

// A move is release on one shard and adopt on the other. Nothing is copied:
// the account, its session, its history and its suspension are where the
// other shard finds them, and each shard's restart agrees on who has it.
func TestMovingATeamBetweenShards(t *testing.T) {
	pg := shardPG(t)
	mk := func(name string) *Shard {
		sh := NewShard(ShardOpts{Senders: byPlatform{}, Secret: "shhh", PG: pg, DataDir: t.TempDir(), Name: name})
		t.Cleanup(sh.Close)
		return sh
	}
	a, b := mk("a"), mk("b")
	provision(t, a, "shhh", "acme", "acme.gliph.chat")
	acme := a.tenantFor("acme.gliph.chat")
	acme.St.Register("waseem", "w@example.com", "hunter2hunter2", a.savedInvite("acme"))
	tok, _, err := acme.St.Login("waseem", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := acme.St.ChannelByName("general")
	acme.St.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "waseem", Body: "c2VhbGVk", Enc: true}, "")

	me := func(sh *Shard) int {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Host = "acme.gliph.chat"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		sh.ServeHTTP(w, r)
		return w.Code
	}
	if c := shardCall(t, b, "/shard/tenants/acme/adopt"); c != 409 {
		t.Fatalf("b adopted a team a still serves: %d", c)
	}
	if c := shardCall(t, a, "/shard/tenants/acme/release"); c != 200 {
		t.Fatalf("release: %d", c)
	}
	if c := me(a); c != 503 {
		t.Fatalf("a moving team on its old shard: %d, want 503", c)
	}
	if c := shardCall(t, b, "/shard/tenants/acme/adopt"); c != 200 {
		t.Fatalf("adopt: %d", c)
	}
	if c := me(b); c != 200 {
		t.Fatalf("the session after the move: %d", c)
	}
	moved := b.tenantFor("acme.gliph.chat")
	if evs, _ := moved.St.Since(ch.ID, 0, 10); len(evs) == 0 || evs[len(evs)-1].Body != "c2VhbGVk" {
		t.Fatalf("history after the move: %+v", evs)
	}
	// Retries of either half change nothing.
	if shardCall(t, a, "/shard/tenants/acme/release") != 200 || shardCall(t, b, "/shard/tenants/acme/adopt") != 200 {
		t.Fatal("a retried half failed")
	}
	// Restarts agree: b has it, a does not.
	for name, want := range map[string]int{"a": 0, "b": 1} {
		again := mk(name)
		if err := again.Load(); err != nil || len(again.Tenants()) != want {
			t.Fatalf("shard %s reloaded %d teams (%v), want %d", name, len(again.Tenants()), err, want)
		}
		again.Close()
	}

	// Moving back works the same way, suspension included, and the old
	// shard stops answering 503.
	r := httptest.NewRequest("POST", "/shard/tenants/acme/status", strings.NewReader(`{"suspended":true,"version":1}`))
	r.Header.Set("Authorization", "Bearer shhh")
	b.ServeHTTP(httptest.NewRecorder(), r)
	if shardCall(t, b, "/shard/tenants/acme/release") != 200 || shardCall(t, a, "/shard/tenants/acme/adopt") != 200 {
		t.Fatal("moving back failed")
	}
	if c := me(a); c != 402 {
		t.Fatalf("the suspension did not move with the team: %d", c)
	}
}
