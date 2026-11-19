package main

import "testing"

// hello.first is true exactly once per account: the first-run tour rides on it.
func TestNeverSeenFlipsOnFirstConnect(t *testing.T) {
	s := testServer(t)
	u, err := s.St.Register("newbie", "newbie@example.com", "Password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.St.NeverSeen(u.ID) {
		t.Fatal("a brand-new account counts as seen")
	}
	s.St.MarkSeen(u.ID)
	if s.St.NeverSeen(u.ID) {
		t.Fatal("a connected account still counts as never seen")
	}
}
