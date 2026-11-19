package main

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The invite is the only way into a closed server, and only an admin can mint
// one. A registration that fails for any other reason must leave it untouched.
func TestFailedRegistrationDoesNotSpendTheInvite(t *testing.T) {
	s := testServer(t)
	if _, err := s.St.CreateUser("boss", "boss@x.io", "hunter2hunter2", "admin"); err != nil {
		t.Fatal(err)
	}
	inv, err := s.St.CreateInvite("member")
	if err != nil {
		t.Fatal(err)
	}

	// Each of these used to burn it, because the invite was claimed before the
	// account was created.
	for _, bad := range []struct {
		name, nick, email, pass string
	}{
		{"password too short", "dana", "d@x.io", "short"},
		{"nick with a space", "da na", "d@x.io", "hunter2hunter2"},
		{"nick already taken", "boss", "d@x.io", "hunter2hunter2"},
	} {
		if _, err := s.St.Register(bad.nick, bad.email, bad.pass, inv); err == nil {
			t.Fatalf("%s: expected the registration to be refused", bad.name)
		}
	}

	u, err := s.St.Register("dana", "d@x.io", "hunter2hunter2", inv)
	if err != nil {
		t.Fatalf("the invite did not survive three failed attempts: %v", err)
	}
	if u.Role != "member" {
		t.Fatalf("role is %q, want member (from the invite)", u.Role)
	}

	// ...and it is spent now that it was actually used.
	if _, err := s.St.Register("robin", "r@x.io", "hunter2hunter2", inv); !errors.Is(err, ErrUsedInvite) {
		t.Fatalf("second use returned %v, want ErrUsedInvite", err)
	}
}

func TestUnknownInviteIsRefused(t *testing.T) {
	s := testServer(t)
	s.St.CreateUser("boss", "boss@x.io", "hunter2hunter2", "admin")
	if _, err := s.St.Register("dana", "d@x.io", "hunter2hunter2", "no-such-token"); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("got %v, want ErrBadInvite", err)
	}
	if _, err := s.St.Register("dana", "d@x.io", "hunter2hunter2", ""); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("blank invite got %v, want ErrBadInvite", err)
	}
}

// One invite, many simultaneous redeemers: exactly one may get an account.
// Checking "is it free" and claiming it used to be two statements, so several
// could pass the check before any of them claimed.
func TestOneInviteAdmitsExactlyOnePerson(t *testing.T) {
	s := testServer(t)
	s.St.CreateUser("boss", "boss@x.io", "hunter2hunter2", "admin")
	inv, err := s.St.CreateInvite("member")
	if err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.St.Register(fmt.Sprintf("racer%d", i), fmt.Sprintf("r%d@x.io", i), "hunter2hunter2", inv)
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d of %d racers got in on one invite, want exactly 1", won, racers)
	}
}

// The first account on an empty server owns it. Deciding that outside a
// transaction let two simultaneous first registrations both come out as admin,
// which hands the server to whoever shows up alongside you.
func TestEmptyServerMintsExactlyOneAdmin(t *testing.T) {
	s := testServer(t)

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	admins, members := 0, 0
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			u, err := s.St.Register(fmt.Sprintf("first%d", i), fmt.Sprintf("f%d@x.io", i), "hunter2hunter2", "")
			if err != nil {
				return
			}
			mu.Lock()
			if u.Role == "admin" {
				admins++
			} else {
				members++
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	if admins != 1 {
		t.Fatalf("%d admins came out of an empty server, want exactly 1", admins)
	}
	// Everyone else needed an invite and had none, so nobody else got in.
	if members != 0 {
		t.Fatalf("%d accounts were created with no invite, want 0", members)
	}
}

// A registration joins the newcomer to #general. Everyone already sitting there
// has to see the roster grow — the join event alone left their member list a
// person short until they reconnected.
func TestRegistrationRefreshesTheRosterOfEveryoneInGeneral(t *testing.T) {
	s := testServer(t)
	boss, err := s.St.CreateUser("boss", "boss@x.io", "hunter2hunter2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, boss.ID, "op")
	bc := fakeConn(s, boss)

	inv, err := s.St.CreateInvite("member")
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"nick":"dana","email":"d@x.io","password":"hunter2hunter2","invite":%q}`, inv)
	req := httptest.NewRequest("POST", "/api/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.Routes("", t.TempDir()).ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}

	var got int
	for _, f := range drain(bc) {
		if f.T != "state" {
			continue
		}
		for _, v := range f.D.(map[string]any)["channels"].([]map[string]any) {
			if v["name"] == "general" {
				got = len(v["members"].([]Member))
			}
		}
	}
	if got != 2 {
		t.Fatalf("boss sees %d members in #general after dana registered, want 2", got)
	}
}
