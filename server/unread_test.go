package main

import "testing"

// What the switcher's bracket counts on a server you are not looking at.
func TestUnreadForCountsPastTheReadMark(t *testing.T) {
	s := testServer(t)
	u, err := s.St.Register("waseem", "w@example.com", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.St.ChannelByName("general")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.St.Join(ch.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}

	// A member of a silent server has nothing waiting.
	if got := s.St.UnreadFor(u.ID); got != 0 {
		t.Fatalf("a silent server: got %d, want 0", got)
	}

	for i := 0; i < 5; i++ {
		if _, err := s.St.Append(&Event{Channel: ch.ID, Kind: "msg", Actor: "robin", Body: "hi"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.St.UnreadFor(u.ID); got != 5 {
		t.Fatalf("five unread: got %d, want 5", got)
	}

	s.St.SaveRead(ch.ID, u.ID, 3)
	if got := s.St.UnreadFor(u.ID); got != 2 {
		t.Fatalf("after reading three: got %d, want 2", got)
	}

	s.St.SaveRead(ch.ID, u.ID, 5)
	if got := s.St.UnreadFor(u.ID); got != 0 {
		t.Fatalf("all caught up: got %d, want 0", got)
	}
}

// A read mark ahead of the log — a device that read, then history was
// trimmed — must not report a negative count that shows up as "[-3]".
func TestUnreadForNeverGoesNegative(t *testing.T) {
	s := testServer(t)
	u, err := s.St.Register("waseem", "w@example.com", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.St.ChannelByName("general")
	if err := s.St.Join(ch.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}
	s.St.SaveRead(ch.ID, u.ID, 99)
	if got := s.St.UnreadFor(u.ID); got != 0 {
		t.Fatalf("a read mark past the log: got %d, want 0", got)
	}
}

// Channels this account is not in are somebody else's unread.
func TestUnreadForOnlyCountsChannelsYouAreIn(t *testing.T) {
	s := testServer(t)
	u, err := s.St.Register("waseem", "w@example.com", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.St.CreateChannel("secret", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.Append(&Event{Channel: other.ID, Kind: "msg", Actor: "robin", Body: "hi"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.St.UnreadFor(u.ID); got != 0 {
		t.Fatalf("a room you are not in: got %d, want 0", got)
	}
}

// History from before you joined is not waiting for you.
func TestUnreadForStartsWhereYouJoined(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	for i := 0; i < 10; i++ {
		s.St.Append(&Event{Channel: ch.ID, Kind: "msg", Actor: "robin", Body: "before"}, "")
	}
	u, _ := s.St.Register("late", "late@example.com", "hunter2hunter2", "")
	s.St.Join(ch.ID, u.ID, "member")
	if got := s.St.UnreadFor(u.ID); got != 0 {
		t.Fatalf("a newcomer got %d unread from before they joined", got)
	}
	s.St.Append(&Event{Channel: ch.ID, Kind: "msg", Actor: "robin", Body: "after"}, "")
	if got := s.St.UnreadFor(u.ID); got != 1 {
		t.Fatalf("one after joining: got %d", got)
	}
}
