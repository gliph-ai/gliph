package main

// Outbound email.
//
// WHAT THIS IS FOR. Everything gliph does in real time it does over the socket,
// and everything it does to a sleeping phone it does with a content-free push
// (push.go). Email is the third case and the slowest one: the user is not here,
// has not been here, and their phone is not going to change that. It is for
// digests and for the handful of account-level messages a server genuinely
// cannot deliver in-band.
//
// THE SAME RULE AS PUSH APPLIES, FOR THE SAME REASON. gliph is end-to-end
// encrypted, so the server cannot write a message preview and must not try. It
// holds ciphertext in an encrypted channel and does not have the key. What it
// legitimately knows is metadata it already stores in plaintext — that there
// are unread messages, how many, and in which channels — and that is the
// ceiling on what any mail this file sends may contain. No bodies, no sender
// lines, no subject lines quoted from a conversation. An email that says "you
// missed 12 messages in #general, come look" is honest; one that shows what was
// said would take the plaintext gliph does not have and post it, in the clear,
// through a third party's SMTP and into the user's mail provider's index.
//
// SENDING IS OFF BY DEFAULT. A self-hosted server with no credentials has no
// mailer and needs none — invites are links the operator sends by hand
// (admin.go). Configure SES and the digest sweep turns on; leave it and
// everything else works exactly as before.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Mailer delivers one message to one address. Implementations are chosen at
// startup and never swapped, so the interface is deliberately this thin: the
// digest is plain text by construction (see the note above — there is nothing
// rich to say), which keeps every adapter to one method and no MIME.
//
// Send is synchronous and returns its error. Push is fire-and-forget because a
// dropped wake costs a notification the phone will pick up on its next sync; a
// dropped digest is the whole message, and the caller needs to know so it does
// not record a send that never happened.
type Mailer interface {
	Send(to, subject, body string) error
}

// noMail is the default: a server nobody configured a mailer for. It reports
// its own absence rather than pretending to send, so callers can skip work.
type noMail struct{}

func (noMail) Send(_, _, _ string) error { return ErrNoMailer }

// ErrNoMailer is what an unconfigured server returns. Callers treat it as "this
// deployment does not do email" and move on quietly — it is a configuration
// state, not a failure.
var ErrNoMailer = errors.New("no mailer configured")

// MailerConfigured reports whether m can actually deliver, so a sweep can skip
// building digests nothing will send.
func MailerConfigured(m Mailer) bool {
	if m == nil {
		return false
	}
	_, isNone := m.(noMail)
	return !isNone
}

// ---- SES ----

// sesMailer talks to the SES v2 HTTP API and signs its own requests. The AWS
// SDK is several hundred packages to make one POST; the signature below is the
// documented SigV4 procedure and fits on a screen. This mirrors what the other
// services on this account already do.
type sesMailer struct {
	keyID  string
	secret string
	region string
	from   string
	// endpoint is the full v2 send URL. A field rather than a constant so the
	// tests can point it at an httptest server and read back what was signed.
	endpoint string
	client   *http.Client
}

// NewSESMailer returns a mailer, or noMail when the credentials are absent, so
// that "unconfigured" is a normal startup path and not an error. It fails hard
// only on configuration that is present but wrong — a half-filled credential or
// an unparseable From is a deployment mistake, and finding out at startup beats
// finding out at the first digest.
func NewSESMailer(keyID, secret, region, from string) (Mailer, error) {
	if keyID == "" && secret == "" {
		return noMail{}, nil
	}
	if keyID == "" || secret == "" {
		return nil, errors.New("ses: need both an access key id and a secret access key")
	}
	if from == "" {
		return nil, errors.New("ses: -mail-from is required when SES credentials are set")
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("ses: -mail-from %q is not a valid address: %w", from, err)
	}
	if region == "" {
		region = "eu-west-1"
	}
	return &sesMailer{
		keyID:    keyID,
		secret:   secret,
		region:   region,
		from:     from,
		endpoint: "https://email." + region + ".amazonaws.com/v2/email/outbound-emails",
		client:   &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (s *sesMailer) Send(to, subject, body string) error {
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("ses: bad recipient %q: %w", to, err)
	}
	// Subject and body are built by this server from its own metadata, never
	// echoed from user content, so there is no header to inject through — the
	// v2 API takes them as JSON fields and does the encoding itself.
	payload, err := json.Marshal(map[string]any{
		"FromEmailAddress": s.from,
		"Destination":      map[string]any{"ToAddresses": []string{to}},
		"Content": map[string]any{
			"Simple": map[string]any{
				"Subject": map[string]any{"Data": subject, "Charset": "UTF-8"},
				"Body": map[string]any{
					"Text": map[string]any{"Data": body, "Charset": "UTF-8"},
				},
			},
		},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.sign(req, payload, time.Now().UTC())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("ses: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// SES puts the useful part in the body: a sandbox account refusing an
		// unverified recipient looks identical to a bad key without it.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ses: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// sign applies AWS Signature Version 4 to req. Kept here rather than pulled in
// because it is a fixed, published procedure: canonical request, string to
// sign, a four-step key derivation, one header.
func (s *sesMailer) sign(req *http.Request, payload []byte, now time.Time) {
	const service = "ses"
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + s.region + "/" + service + "/aws4_request"

	req.Header.Set("X-Amz-Date", amzDate)
	host := req.URL.Host
	payloadHash := sha256Hex(payload)

	// Signed headers must be lowercase and sorted; these three already are.
	canonicalHeaders := "content-type:" + req.Header.Get("Content-Type") + "\n" +
		"host:" + host + "\n" +
		"x-amz-date:" + amzDate + "\n"
	const signedHeaders = "content-type;host;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.Path,
		"", // no query string
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secret), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.keyID, scope, signedHeaders, signature))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- the missed-message digest ----
//
// The rule, in one place: while you are away and things are being said, gliph
// mails you once. Not once per message, not once per channel, and not again
// until you have actually come back and left again. A person who never returns
// gets exactly one email, ever — which is the behaviour you want from a chat
// server that has your address.
//
// Counted against the READ mark, not the delivery cursor. A phone that wakes in
// the background, syncs fifty messages and drops again without anyone looking
// has been delivered them and has read none of them; counting delivery would
// keep this quiet about exactly the messages it exists to report.
//
// Three conditions gate it, and all three have to hold:
//
//	quiet      you have been gone at least digestQuiet. Closing a laptop is not
//	           being away, and push (push.go) is the right tool for the first
//	           hour regardless.
//	seen since you have connected at least once since the last digest. This is
//	           the "nothing more until you are back" half, and it is what keeps
//	           a long absence from turning into a daily mailing.
//	one a day  and even then, never twice within digestInterval, so an account
//	           that connects and drops repeatedly cannot be made to flood.
//
// WHAT IT MAY SAY is settled at the top of this file: counts and channel names.
// Direct messages are counted but never attributed. A room name is shared by
// everyone in it; "a DM from alice" is a social-graph edge, and handing that to
// the recipient's mail provider is exactly the kind of leak the rest of gliph
// spends its effort avoiding. The count is enough to make someone look.
const (
	digestQuiet    = time.Hour
	digestInterval = 24 * time.Hour
)

// ---- store ----

// digestRow is one channel with unread messages in it, for one user. The query
// returns every (user, channel) pair that qualifies; the sweep groups them.
type digestRow struct {
	UID     string
	Email   string
	Channel string
	IsDM    bool
	Count   int
}

// DigestRows finds everyone owed a digest and what they missed, in one query
// rather than one per user per channel.
//
// quietBefore and digestBefore are RFC3339 stamps: last_seen must be older than
// the first (you have been gone a while) and last_digest older than the second
// (we have not just mailed you). An empty stamp sorts below both, which is how
// an account that has never been mailed passes and one that has never connected
// does not.
func (s *Store) DigestRows(quietBefore, digestBefore string) ([]digestRow, error) {
	rows, err := s.r.Query(`
		SELECT u.id, u.email, c.name, c.is_dm, COUNT(*)
		FROM users u
		JOIN members m ON m.user_id = u.id
		JOIN channels c ON c.id = m.channel_id
		JOIN events e ON e.channel_id = m.channel_id
		WHERE u.suspended = 0
		  AND u.email <> ''
		  AND u.last_seen <> '' AND u.last_seen < ?
		  AND (u.last_digest = '' OR u.last_digest < u.last_seen)
		  AND (u.last_digest = '' OR u.last_digest < ?)
		  AND e.seq > m.read_seq
		  AND e.seq > m.joined_seq
		  AND e.kind IN ('message','action','file')
		  AND e.actor <> u.nick
		GROUP BY u.id, c.id
		ORDER BY u.id, c.is_dm, c.name`, quietBefore, digestBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []digestRow
	for rows.Next() {
		var r digestRow
		var dm int
		if err := rows.Scan(&r.UID, &r.Email, &r.Channel, &dm, &r.Count); err != nil {
			return nil, err
		}
		r.IsDM = dm == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkSeen records that an account is, or just was, connected. Called on the
// first connection and the last disconnection: between those two the user is
// online and the digest skips them anyway, so there is nothing to write.
func (s *Store) MarkSeen(uid string) {
	s.db.Exec(`UPDATE users SET last_seen=? WHERE id=?`, now(), uid)
}

// NeverSeen: this account has never connected. Read before the connection's
// MarkSeen, it is what makes a hello the account's first (hello.first, the
// first-run tour in core/src/client.rs).
func (s *Store) NeverSeen(uid string) bool {
	var seen string
	s.db.QueryRow(`SELECT last_seen FROM users WHERE id=?`, uid).Scan(&seen)
	return seen == ""
}

// MarkDigested stamps a send. Written after the mail is accepted, never before:
// a stamp on a message that failed to send would silence the next sweep too,
// and the user would simply never hear about any of it.
func (s *Store) MarkDigested(uid string) {
	s.db.Exec(`UPDATE users SET last_digest=? WHERE id=?`, now(), uid)
}

// ---- the sweep ----

// sendDigests mails everyone who qualifies. Runs on the housekeeping ticker in
// main.go; does nothing at all on a server with no mailer configured.
func (s *Server) sendDigests() {
	if !MailerConfigured(s.Mail) {
		return
	}
	at := time.Now().UTC()
	rows, err := s.St.DigestRows(
		at.Add(-digestQuiet).Format(time.RFC3339),
		at.Add(-digestInterval).Format(time.RFC3339))
	if err != nil {
		log.Printf("digest: %v", err)
		return
	}
	// Rows arrive grouped by user. Anyone who reconnected between the query and
	// here is dropped: last_seen only moves on connect and disconnect, so a user
	// who has been online for hours still looks stale to SQL and this is the
	// check that catches them.
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].UID == rows[i].UID {
			j++
		}
		user, missed := rows[i], rows[i:j]
		i = j

		if s.IsOnline(user.UID) {
			continue
		}
		subject, body := digestMessage(s.Name, missed)
		if err := s.Mail.Send(user.Email, subject, body); err != nil {
			log.Printf("digest: %s: %v", user.Email, err)
			continue
		}
		s.St.MarkDigested(user.UID)
	}
}

// digestMessage writes the mail. Counts and room names only — see the note at
// the top of this file for why that is a ceiling and not a first draft.
func digestMessage(serverName string, missed []digestRow) (subject, body string) {
	total, dms := 0, 0
	var rooms []digestRow
	for _, r := range missed {
		total += r.Count
		if r.IsDM {
			dms += r.Count
			continue
		}
		rooms = append(rooms, r)
	}

	subject = fmt.Sprintf("%s while you were away", messages(total))

	var b strings.Builder
	b.WriteString("You missed:\n\n")
	for _, r := range rooms {
		fmt.Fprintf(&b, "  #%-20s %s\n", r.Channel, messages(r.Count))
	}
	if dms > 0 {
		fmt.Fprintf(&b, "  %-21s %s\n", "direct messages", messages(dms))
	}
	fmt.Fprintf(&b, "\nRead them at https://%s\n", serverName)
	// The cadence is the feature, so it is stated rather than left to be
	// discovered. Someone who gets this once should know what stops it.
	b.WriteString("\nYou will not get another email until you have signed in again.\n")
	return subject, b.String()
}

// messages renders a count the way the digest says it everywhere: "1 message",
// "8 messages". plural() in devlog.go supplies the s.
func messages(n int) string {
	return fmt.Sprintf("%d message%s", n, plural(n))
}
