package main

import (
	"encoding/json"
	"testing"
)

func readFrame(t *testing.T, s *Server, c *Conn, marks map[string]int64) {
	t.Helper()
	d, _ := json.Marshal(map[string]any{"marks": marks})
	raw, _ := json.Marshal(map[string]any{"t": "read", "d": json.RawMessage(d)})
	s.handleFrame(c, raw)
}

// Read state is per account, not per device, and the relay is what makes that
// true while both are open.
func TestReadMarkReachesTheOtherDevicesAndNotTheSender(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	me, err := s.St.CreateUser("waseem", "w@example.com", "password1", "member")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.St.CreateUser("robin", "r@example.com", "password1", "member")
	if err != nil {
		t.Fatal(err)
	}
	s.St.Join(ch.ID, me.ID, "member")
	s.St.Join(ch.ID, other.ID, "member")
	// Something to have read: marks are clamped to the end of the log.
	for i := 0; i < 10; i++ {
		s.St.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "robin", Body: "hi"}, "")
	}

	phone := fakeConn(s, me)
	desktop := fakeConn(s, me)
	theirs := fakeConn(s, other)
	drainCh(phone.out)
	drainCh(desktop.out)
	drainCh(theirs.out)

	readFrame(t, s, phone, map[string]int64{ch.ID: 7})

	if got := s.St.ReadSeq(ch.ID, me.ID); got != 7 {
		t.Fatalf("read mark stored as %d, want 7", got)
	}
	if got := framesOfType(drainCh(desktop.out), "read"); len(got) != 1 {
		t.Fatalf("the account's other device got %d read frames, want 1", len(got))
	}
	if got := framesOfType(drainCh(phone.out), "read"); len(got) != 0 {
		t.Errorf("the device that told us was echoed back to: %d frames", len(got))
	}
	if got := framesOfType(drainCh(theirs.out), "read"); len(got) != 0 {
		t.Errorf("someone else's device was told where we had read to: %d frames", len(got))
	}
}

// Forward only. A device that has been offline comes back holding an old mark;
// honouring it would un-read what another device already read.
func TestReadMarkNeverMovesBackwards(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	me, _ := s.St.CreateUser("waseem", "w@example.com", "password1", "member")
	s.St.Join(ch.ID, me.ID, "member")
	// Real events to read: a mark is clamped to the end of the log, so a
	// channel with nothing in it cannot hold one.
	for i := 0; i < 25; i++ {
		s.St.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "robin", Body: "hi"}, "")
	}
	c := fakeConn(s, me)
	drainCh(c.out)

	readFrame(t, s, c, map[string]int64{ch.ID: 20})
	readFrame(t, s, c, map[string]int64{ch.ID: 5})

	if got := s.St.ReadSeq(ch.ID, me.ID); got != 20 {
		t.Fatalf("a stale mark moved the read position to %d", got)
	}
}

// A channel id from a client is a claim, not a fact — the same rule every other
// channel entry point follows.
func TestReadMarkForAChannelYouAreNotInIsRefused(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	outsider, _ := s.St.CreateUser("nosy", "n@example.com", "password1", "member")
	for i := 0; i < 10; i++ {
		s.St.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "robin", Body: "hi"}, "")
	}
	c := fakeConn(s, outsider)
	drainCh(c.out)

	readFrame(t, s, c, map[string]int64{ch.ID: 99})

	if got := s.St.ReadSeq(ch.ID, outsider.ID); got != 0 {
		t.Fatalf("a non-member wrote a read mark: %d", got)
	}
}

// The state frame is how a device that has been away catches up, so the mark
// has to be on it.
func TestChannelViewCarriesTheReadMark(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	me, _ := s.St.CreateUser("waseem", "w@example.com", "password1", "member")
	s.St.Join(ch.ID, me.ID, "member")
	s.St.SaveRead(ch.ID, me.ID, 12)

	v := s.channelView(ch, me.ID)
	if v["read"] != int64(12) {
		t.Fatalf("channel view read mark is %v, want 12", v["read"])
	}
}

// A read mark past the end of the log is not a read position. Storing one is
// permanent damage: unread floors at zero and the digest counts nothing in that
// channel from then on.
func TestReadMarkCannotRunPastTheEndOfTheChannel(t *testing.T) {
	s := testServer(t)
	ch, _ := s.St.ChannelByName("general")
	me, _ := s.St.CreateUser("waseem", "w@example.com", "password1", "member")
	s.St.Join(ch.ID, me.ID, "member")
	for i := 0; i < 3; i++ {
		s.St.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "robin", Body: "hi"}, "")
	}
	c := fakeConn(s, me)
	drainCh(c.out)

	readFrame(t, s, c, map[string]int64{ch.ID: 9_999})

	max, _ := s.St.MaxSeq(ch.ID)
	if got := s.St.ReadSeq(ch.ID, me.ID); got != max {
		t.Fatalf("read mark stored as %d, want it clamped to the newest event %d", got, max)
	}
}
