package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One check over the guards that keep an operator from locking everyone out,
// plus proof that suspension drops sessions and that no listing leaks a hash.
func TestAdminConsole(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())

	boss, _ := s.St.CreateUser("boss", "boss@x", "password1", "admin")
	mem, _ := s.St.CreateUser("mem", "mem@x", "password1", "member")
	tok, _, err := s.St.Login("boss", "password1")
	if err != nil {
		t.Fatal(err)
	}
	memTok, _, _ := s.St.Login("mem", "password1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// non-admin gets nothing
	r := httptest.NewRequest("GET", "/api/admin/users", nil)
	r.Header.Set("Authorization", "Bearer "+memTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("member reached the admin API: %d", w.Code)
	}

	if w := do("GET", "/api/admin/users", ""); w.Code != 200 || strings.Contains(w.Body.String(), "$2a$") {
		t.Fatalf("users listing bad (%d) or leaking a hash: %s", w.Code, w.Body)
	}

	// last admin: neither demotable nor deletable
	if w := do("POST", "/api/admin/users/"+boss.ID+"/role", `{"role":"member"}`); w.Code != 409 {
		t.Fatalf("demoted the last admin: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/api/admin/users/"+boss.ID, ""); w.Code != 409 {
		t.Fatalf("deleted the last admin: %d %s", w.Code, w.Body)
	}

	// with a second admin, self-demotion is still refused
	do("POST", "/api/admin/users/"+mem.ID+"/role", `{"role":"admin"}`)
	if w := do("POST", "/api/admin/users/"+boss.ID+"/role", `{"role":"member"}`); w.Code != 409 {
		t.Fatalf("self-demotion allowed: %d %s", w.Code, w.Body)
	}

	// suspension kills the session and blocks login
	if w := do("POST", "/api/admin/users/"+mem.ID+"/suspend", ""); w.Code != 200 {
		t.Fatalf("suspend: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.UserByToken(memTok); err == nil {
		t.Fatal("suspended user's token still works")
	}
	if _, _, err := s.St.Login("mem", "password1"); err == nil {
		t.Fatal("suspended user could log in")
	}

	// archive empties the channel and locks it
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, boss.ID, "op")
	if w := do("POST", "/api/admin/channels/"+ch.ID+"/archive", ""); w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	if m, _ := s.St.Members(ch.ID); len(m) != 0 {
		t.Fatalf("archived channel still has %d members", len(m))
	}
	if ch, _ = s.St.ChannelByID(ch.ID); !ch.HasMode('i') {
		t.Fatal("archived channel is still joinable")
	}

	// transfer makes the nick an op again
	if w := do("POST", "/api/admin/channels/"+ch.ID+"/transfer", `{"nick":"boss"}`); w.Code != 200 {
		t.Fatalf("transfer: %d %s", w.Code, w.Body)
	}
	if role, ok := s.St.MemberRole(ch.ID, boss.ID); !ok || role != "op" {
		t.Fatalf("transfer left role %q (member=%v)", role, ok)
	}

	var stats map[string]any
	if err := json.Unmarshal(do("GET", "/api/admin/stats", "").Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats["users"] == nil || stats["uptime_seconds"] == nil {
		t.Fatalf("stats missing fields: %v", stats)
	}
}

// The audit row has to outlive its subject: that is the whole reason `target`
// is text and not a foreign key. Delete the user, then prove the record of the
// deletion still names them.
func TestAuditLogSurvivesItsSubject(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())

	boss, _ := s.St.CreateUser("boss", "boss@x", "password1", "admin")
	doomed, _ := s.St.CreateUser("doomed", "doomed@x", "password1", "member")
	dm, _ := s.DMChannel(boss, doomed)
	tok, _, err := s.St.Login("boss", "password1")
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := do("DELETE", "/api/admin/users/"+doomed.ID); w.Code != 200 {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.UserByID(doomed.ID); err == nil {
		t.Fatal("user still present after delete")
	}
	if _, err := s.St.ChannelByID(dm.ID); err == nil {
		t.Fatal("the DM with the deleted user survived")
	}

	w := do("GET", "/api/admin/audit")
	if w.Code != 200 {
		t.Fatalf("audit listing: %d", w.Code)
	}
	var rows []AuditEntry
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range rows {
		if e.Action == "delete-user" && e.Target == "doomed" && e.Actor == "boss" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deletion of a since-removed user left no audit row: %+v", rows)
	}
}

// Invites and plugins are the two things an operator hands out: a link that
// creates an account and a token that lets a bot in. Both must be revocable,
// and a redeemed invite must survive as the record of how an account got here.
func TestAdminInvitesAndPlugins(t *testing.T) {
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

	var inv struct{ Invite, Role string }
	w := do("POST", "/api/admin/invites", `{"role":"member"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil || inv.Invite == "" {
		t.Fatalf("create invite: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/api/admin/invites/"+inv.Invite, ""); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	// Redeemed through Register, which is the only path that spends an invite —
	// the store has no separate redeem method to test against any more.
	if _, err := s.St.Register("someone", "s@x", "password1", inv.Invite); err == nil {
		t.Fatal("a revoked invite still works")
	}

	// a redeemed one is not revocable and stays in the listing, named
	used := do("POST", "/api/admin/invites", `{"role":"member"}`)
	json.Unmarshal(used.Body.Bytes(), &inv)
	if _, err := s.St.Register("newbie", "n@x", "password1", inv.Invite); err != nil {
		t.Fatal(err)
	}
	if w := do("DELETE", "/api/admin/invites/"+inv.Invite, ""); w.Code != 409 {
		t.Fatalf("revoked a redeemed invite: %d %s", w.Code, w.Body)
	}
	var list []map[string]any
	json.Unmarshal(do("GET", "/api/admin/invites", "").Body.Bytes(), &list)
	// one row: the revoked invite is gone, the redeemed one names its user
	if len(list) != 1 || !strings.Contains(do("GET", "/api/admin/invites", "").Body.String(), `"used_by":"newbie"`) {
		t.Fatalf("invite listing wrong: %v", list)
	}
	// ...and stays redeemed after that user is deleted (prod showed such an
	// invite as open, with a revoke button that 409ed)
	nb, _ := s.St.UserByNick("newbie")
	if w := do("DELETE", "/api/admin/users/"+nb.ID, ""); w.Code != 200 {
		t.Fatalf("delete newbie: %d %s", w.Code, w.Body)
	}
	if body := do("GET", "/api/admin/invites", "").Body.String(); !strings.Contains(body, `"used_by":"a deleted account"`) {
		t.Fatalf("redeemed invite shows as open after its user was deleted: %s", body)
	}

	// plugins: the token comes back once, on registration, and never in a list
	var p Plugin
	w = do("POST", "/api/admin/plugins", `{"id":"standup","display":"Standup","kind":"bot"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Token == "" {
		t.Fatalf("register plugin: %d %s", w.Code, w.Body)
	}
	if body := do("GET", "/api/admin/plugins", "").Body.String(); strings.Contains(body, p.Token) {
		t.Fatalf("plugin listing leaks the token: %s", body)
	}
	if w := do("DELETE", "/api/admin/plugins/standup", ""); w.Code != 200 {
		t.Fatalf("revoke plugin: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.PluginByToken(p.Token); err == nil {
		t.Fatal("a revoked plugin's token still authenticates")
	}
}

// The console's provisioning half: an operator who already knows who is joining
// creates the account outright, and resets a password when someone loses one.
// A reset that left the old sessions alive would have reset nothing, so that is
// what this pins.
func TestAdminProvisioning(t *testing.T) {
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

	if w := do("POST", "/api/admin/users", `{"nick":"robin","email":"r@x","password":"password1","role":"member"}`); w.Code != 200 {
		t.Fatalf("create account: %d %s", w.Code, w.Body)
	}
	robinTok, robin, err := s.St.Login("robin", "password1")
	if err != nil {
		t.Fatalf("provisioned account cannot log in: %v", err)
	}
	// It lands where a self-registered account lands, not in an empty server.
	if ch, err := s.St.ChannelByName("general"); err != nil {
		t.Fatal(err)
	} else if _, ok := s.St.MemberRole(ch.ID, robin.ID); !ok {
		t.Fatal("provisioned account is not in #general")
	}
	if w := do("POST", "/api/admin/users", `{"nick":"robin","email":"other@x","password":"password1"}`); w.Code != 400 {
		t.Fatalf("duplicate nick accepted: %d %s", w.Code, w.Body)
	}
	// A password shorter than the floor is the store's rule, not the console's.
	if w := do("POST", "/api/admin/users/"+robin.ID+"/password", `{"password":"short"}`); w.Code != 400 {
		t.Fatalf("short password accepted: %d %s", w.Code, w.Body)
	}

	if w := do("POST", "/api/admin/users/"+robin.ID+"/password", `{"password":"password2"}`); w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.UserByToken(robinTok); err == nil {
		t.Fatal("a password reset left the old session alive")
	}
	if _, _, err := s.St.Login("robin", "password2"); err != nil {
		t.Fatalf("new password does not work: %v", err)
	}

	if w := do("POST", "/api/admin/channels", `{"name":"support","topic":"ask here"}`); w.Code != 200 {
		t.Fatalf("create channel: %d %s", w.Code, w.Body)
	}
	if _, err := s.St.ChannelByName("support"); err != nil {
		t.Fatalf("created channel missing: %v", err)
	}
}

// The dashboard's charts. Two rules: the window is exactly the requested number
// of days, and a day with nothing in it is a zero in the array — a chart that
// silently skips quiet days draws a lie.
func TestAdminSeriesIsZeroFilled(t *testing.T) {
	s := testServer(t)
	h := s.Routes("", t.TempDir())

	s.St.CreateUser("boss", "boss@x", "password1", "admin")
	tok, _, err := s.St.Login("boss", "password1")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/admin/series?days=7", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("series: %d %s", w.Code, w.Body)
	}
	var got struct {
		Days                              []string
		Signups, Messages, Files, Actions []int64
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for name, series := range map[string][]int64{
		"signups": got.Signups, "messages": got.Messages, "files": got.Files, "actions": got.Actions,
	} {
		if len(series) != 7 {
			t.Fatalf("%s has %d points, want 7 (quiet days must still be plotted)", name, len(series))
		}
	}
	if len(got.Days) != 7 || got.Days[6] != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("day labels wrong, last should be today: %v", got.Days)
	}
	// boss registered just now, on the last day of the window.
	if got.Signups[6] != 1 {
		t.Fatalf("today's signup not counted: %v", got.Signups)
	}
}

// Two things the admin surface must not do: accept its token from the URL,
// where every proxy log and Referer header would collect it, and 404 the
// console's own deep links.
func TestAdminTokenIsHeaderOnlyAndPageRoutes(t *testing.T) {
	s := testServer(t)
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "admin.html"), []byte("<!doctype html>console"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html>client"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := s.Routes(web, t.TempDir())

	s.St.CreateUser("boss", "boss@x", "password1", "admin")
	tok, _, err := s.St.Login("boss", "password1")
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/api/admin/users?token=" + tok); w.Code != 403 {
		t.Fatalf("admin API accepted a token from the query string: %d %s", w.Code, w.Body)
	}
	// /metrics rides the same wrapper, so it inherits the same rule.
	if w := get("/metrics?token=" + tok); w.Code != 403 {
		t.Fatalf("/metrics accepted a token from the query string: %d", w.Code)
	}

	for _, p := range []string{"/admin", "/admin/", "/admin/users", "/admin/dashboard"} {
		w := get(p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "console") {
			t.Fatalf("%s did not serve the console: %d %q", p, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%s is framable: %q", p, w.Header().Get("Content-Security-Policy"))
		}
	}
	// and the client still owns everything else
	if w := get("/some/channel"); !strings.Contains(w.Body.String(), "client") {
		t.Fatalf("SPA fallback broken: %q", w.Body.String())
	}
}
