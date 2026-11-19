package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMail struct{ to, subject, body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	fail error
}

func (m *fakeMailer) Send(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *fakeMailer) last() sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return sentMail{}
	}
	return m.sent[len(m.sent)-1]
}

// digestFixture: one channel, one member who has gone away, and a mailer that
// records rather than sends.
func digestFixture(t *testing.T) (*Server, *fakeMailer, *User, string) {
	t.Helper()
	s := testServer(t)
	m := &fakeMailer{}
	s.Mail = m

	ch, err := s.St.ChannelByName("general")
	if err != nil {
		t.Fatal(err)
	}
	away, err := s.St.CreateUser("away", "away@example.com", "password1", "member")
	if err != nil {
		t.Fatal(err)
	}
	talker, err := s.St.CreateUser("talker", "talker@example.com", "password1", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.St.Join(ch.ID, away.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.St.Join(ch.ID, talker.ID, "member"); err != nil {
		t.Fatal(err)
	}
	return s, m, away, ch.ID
}

// wentAway backdates the account's last_seen, which is the only clock the
// digest reads. Cheaper and clearer than injecting a fake one.
func wentAway(t *testing.T, s *Server, uid string, ago time.Duration) {
	t.Helper()
	stamp := time.Now().UTC().Add(-ago).Format(time.RFC3339)
	if _, err := s.St.db.Exec(`UPDATE users SET last_seen=? WHERE id=?`, stamp, uid); err != nil {
		t.Fatal(err)
	}
}

func say(t *testing.T, s *Server, cid, nick, body string) {
	t.Helper()
	if _, err := s.St.Append(&Event{Channel: cid, Kind: "message", Actor: nick, Body: body}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDigestMailsWhatWasMissed(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")
	say(t, s, cid, "talker", "two")

	s.sendDigests()

	if m.count() != 1 {
		t.Fatalf("want exactly one digest, got %d", m.count())
	}
	got := m.last()
	if got.to != "away@example.com" {
		t.Errorf("sent to %q", got.to)
	}
	if !strings.Contains(got.subject, "2 messages") {
		t.Errorf("subject does not carry the count: %q", got.subject)
	}
	if !strings.Contains(got.body, "#general") || !strings.Contains(got.body, "2 messages") {
		t.Errorf("body does not name the room and the count:\n%s", got.body)
	}
	// The whole point of the file: the server holds ciphertext and must not
	// quote it, so no message body may ever appear in the mail.
	if strings.Contains(got.body, "one") || strings.Contains(got.body, "two") {
		t.Errorf("digest leaked message content:\n%s", got.body)
	}
}

// The rule the user asked for: one mail per absence. Not one a day forever, and
// not another one just because more arrived.
func TestDigestStaysSilentUntilSeenAgain(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")

	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("first sweep should mail once, got %d", m.count())
	}

	// More arrives, and more sweeps run. Still nothing: they have not come back.
	say(t, s, cid, "talker", "two")
	s.sendDigests()
	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("a second digest went out before the user returned: %d total", m.count())
	}

	// Push the whole history back past the once-a-day cap so that gate is wide
	// open and only the seen-again rule is under test. The user still has not
	// returned — last_seen is older than the digest we sent them.
	digestAt := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	s.St.db.Exec(`UPDATE users SET last_digest=? WHERE id=?`, digestAt, away.ID)
	wentAway(t, s, away.ID, 26*time.Hour)
	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("a day passed but the user never came back, and gliph mailed anyway: %d", m.count())
	}

	// They sign in and leave again. Now both gates are open.
	s.St.MarkSeen(away.ID)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "three")
	s.sendDigests()
	if m.count() != 2 {
		t.Fatalf("coming back and leaving again should re-arm the digest, got %d", m.count())
	}
}

// Even a user who reconnects and drops repeatedly cannot be made to flood: the
// 24-hour cap is the backstop behind the seen-again rule.
func TestDigestNeverTwiceInADay(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")
	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("setup: want one, got %d", m.count())
	}

	// Seen again (so the seen-again gate opens) but the last digest was minutes
	// ago, not a day.
	s.St.MarkSeen(away.ID)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "two")
	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("digest ignored the once-a-day cap: %d sent", m.count())
	}

	// Backdate the send past the cap and the same state now qualifies.
	stamp := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	s.St.db.Exec(`UPDATE users SET last_digest=? WHERE id=?`, stamp, away.ID)
	s.sendDigests()
	if m.count() != 2 {
		t.Fatalf("past the cap the digest should send again, got %d", m.count())
	}
}

func TestDigestLeavesTheRecentlyGoneAlone(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 5*time.Minute) // inside digestQuiet
	say(t, s, cid, "talker", "one")

	s.sendDigests()
	if m.count() != 0 {
		t.Fatalf("someone who just closed a tab was mailed: %d", m.count())
	}
}

func TestDigestSkipsUsersWhoAreOnline(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	// Stale last_seen, but the socket is up: a user connected for hours still
	// looks away to SQL, and only the liveness check catches it.
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")
	fakeConn(s, away)

	s.sendDigests()
	if m.count() != 0 {
		t.Fatalf("mailed a user who is connected right now: %d", m.count())
	}
}

// Joins, parts and topic changes are not things you missed. Counting them turns
// "you missed 40 messages" into a lie about an empty room.
func TestDigestCountsOnlyHumanUtterances(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "the only real one")
	for _, k := range []string{"join", "part", "topic", "mode", "notice"} {
		if _, err := s.St.Append(&Event{Channel: cid, Kind: k, Actor: "talker"}, ""); err != nil {
			t.Fatal(err)
		}
	}

	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("want one digest, got %d", m.count())
	}
	if !strings.Contains(m.last().subject, "1 message") {
		t.Errorf("room noise was counted as messages: %q", m.last().subject)
	}
}

func TestDigestIgnoresYourOwnMessages(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "away", "posted then closed the tab")

	s.sendDigests()
	if m.count() != 0 {
		t.Fatalf("mailed someone about their own message: %q", m.last().body)
	}
}

// A DM is counted but never attributed: the room name is an internal id, and
// who is talking to whom is the one thing the mail must not hand to a mail
// provider. See the note in email.go.
func TestDigestCountsDMsWithoutNamingAnyone(t *testing.T) {
	s, m, away, _ := digestFixture(t)
	other, err := s.St.UserByNick("talker")
	if err != nil {
		t.Fatal(err)
	}
	dm, err := s.DMChannel(away, other)
	if err != nil {
		t.Fatal(err)
	}
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, dm.ID, "talker", "hello")

	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("want one digest, got %d", m.count())
	}
	body := m.last().body
	if !strings.Contains(body, "direct message") {
		t.Errorf("DM was not reported at all:\n%s", body)
	}
	if strings.Contains(body, "talker") {
		t.Errorf("digest named the other side of a DM:\n%s", body)
	}
	if strings.Contains(body, dm.ID) || strings.Contains(body, "dm:") {
		t.Errorf("digest leaked the internal DM channel name:\n%s", body)
	}
}

// A send that fails must not be recorded as a send: stamping it would silence
// the next sweep too, and the user would never hear about any of it.
func TestFailedDigestIsRetriedNotSwallowed(t *testing.T) {
	s, m, away, cid := digestFixture(t)
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")

	m.fail = errors.New("SES is having a day")
	s.sendDigests()
	if m.count() != 0 {
		t.Fatal("setup: the failing mailer recorded a send")
	}

	m.fail = nil
	s.sendDigests()
	if m.count() != 1 {
		t.Fatalf("a failed digest was never retried: %d", m.count())
	}
}

// A server with no credentials must not query, stamp, or send anything.
func TestDigestIsInertWithoutAMailer(t *testing.T) {
	s, _, away, cid := digestFixture(t)
	s.Mail = noMail{}
	wentAway(t, s, away.ID, 2*time.Hour)
	say(t, s, cid, "talker", "one")

	s.sendDigests()

	var stamp string
	s.St.db.QueryRow(`SELECT last_digest FROM users WHERE id=?`, away.ID).Scan(&stamp)
	if stamp != "" {
		t.Errorf("an unconfigured server stamped a digest it never sent: %q", stamp)
	}
}

// Deploying the digest onto a live server must not mail everyone at once.
// Existing accounts carry no last_seen until they next connect, and an account
// that has never been seen is not owed a digest for history it may already have
// read on a device that never told us.
func TestDigestDoesNotBlastAccountsItHasNeverSeen(t *testing.T) {
	s, m, _, cid := digestFixture(t)
	// No wentAway() here: last_seen is empty, exactly as it is for every account
	// that existed before the migration ran.
	say(t, s, cid, "talker", "one")
	say(t, s, cid, "talker", "two")

	s.sendDigests()
	if m.count() != 0 {
		t.Fatalf("a never-seen account was mailed on the first sweep: %d", m.count())
	}
}
