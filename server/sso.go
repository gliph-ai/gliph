package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Signing in to a hosted team with the account you signed up with.
//
// The control plane knows which teams an account belongs to; the shard knows
// nothing about accounts and never will. So the control plane hands the client
// a short-lived assertion — "this email may be <nick> on acme" — signed with a
// key only it holds, and the shard trades it for an ordinary session token.
//
// What the shard does NOT do is trust the client. The assertion is signed, it
// names the team it is for, and it expires in a minute; a shard with no
// -control-key configured refuses every one of them, which is what a
// self-hosted server wants. Self-hosted teams keep passwords and invites, and
// nothing about this path is available to them.

// ssoClaims is the whole assertion. Small on purpose: every field here is
// something the shard acts on.
type ssoClaims struct {
	// Aud is the tenant slug this assertion is for. Without it, an assertion
	// minted for one team would be a session on any team on the shard.
	Aud   string `json:"aud"`
	Email string `json:"email"`
	Nick  string `json:"nick"`
	// Exp is unix seconds. A minute is plenty for a redirect and leaves a
	// stolen assertion worth almost nothing.
	Exp int64 `json:"exp"`
	// Jti is the assertion's own id. It is spent on first redemption
	// (Store.SpendAssertion), so a copied assertion mints nothing.
	Jti string `json:"jti"`
	// Role is the account's role in the team on the control plane. An
	// "owner" is this server's admin: whoever created the team runs it.
	Role string `json:"role,omitempty"`
}

var errNoSSO = errors.New("this server does not accept single sign-on")

// verifyAssertion checks the signature, the clock and the audience, in that
// order, and hands back what the control plane said.
func verifyAssertion(pub ed25519.PublicKey, slug, assertion string, now time.Time) (*ssoClaims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errNoSSO
	}
	body, sig, ok := strings.Cut(assertion, ".")
	if !ok {
		return nil, errors.New("malformed assertion")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errors.New("malformed assertion")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, errors.New("malformed assertion")
	}
	// Signature first: nothing inside is worth reading until it is known to
	// have come from the control plane.
	if !ed25519.Verify(pub, raw, sigBytes) {
		return nil, errors.New("assertion is not signed by the control plane")
	}
	var c ssoClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("malformed assertion")
	}
	if c.Exp <= now.Unix() {
		return nil, errors.New("assertion has expired")
	}
	// The audience is what stops an assertion for one team being a session on
	// its neighbour, which is the whole reason a shard can be trusted with more
	// than one.
	if c.Aud == "" || c.Aud != slug {
		return nil, fmt.Errorf("assertion is for %q, not this server", c.Aud)
	}
	if c.Email == "" {
		return nil, errors.New("assertion names nobody")
	}
	if c.Jti == "" {
		return nil, errors.New("assertion has no id")
	}
	return &c, nil
}

// SpendAssertion records an assertion's id, once. The primary key is the
// check: a second redemption of the same assertion fails the insert. Rows go
// when the assertion would have expired anyway (PruneSpentAssertions).
func (s *Store) SpendAssertion(jti string, exp int64) error {
	if _, err := s.db.Exec(`INSERT INTO sso_spent(jti,exp) VALUES(?,?)`, jti, exp); err != nil {
		return errors.New("assertion already used")
	}
	return nil
}

func (s *Store) PruneSpentAssertions(now time.Time) {
	s.db.Exec(`DELETE FROM sso_spent WHERE exp < ?`, now.Unix())
}

// SSOSession finds or creates the local account for an email the control plane
// vouched for, and mints an ordinary session for it. Everything downstream —
// every channel, every permission check — sees a normal user row, because it is
// one.
func (s *Store) SSOSession(email, wantNick string, owner bool) (string, *User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", nil, errors.New("no email")
	}
	// Only an account single sign-on itself created is ever signed in to by
	// email. A password account with the same address stays its owner's: the
	// control plane does not prove an address, so trusting it here would let
	// whoever registered an admin's email there become that admin here.
	var sso int
	u := &User{}
	err := s.db.QueryRow(`SELECT id,nick,email,role,away,suspended,sso FROM users WHERE email=?`, email).
		Scan(&u.ID, &u.Nick, &u.Email, &u.Role, &u.Away, &u.Susp, &sso)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		u, err = s.createSSOUser(email, wantNick)
	case err == nil && sso != 1:
		return "", nil, errors.New("this email already has a password account on this team; sign in with its password")
	}
	if err != nil {
		return "", nil, err
	}
	// The control plane is the authority on who owns the team, so an owner is
	// made admin here, on the first sign-in and on any after a promotion. It
	// never demotes: a local admin made here stays one.
	if owner && u.Role != "admin" {
		if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, u.ID); err != nil {
			return "", nil, err
		}
		u.Role = "admin"
	}
	if u.Susp {
		return "", nil, errors.New("account suspended")
	}
	t := token()
	if _, err := s.db.Exec(`INSERT INTO sessions(token,user_id,created_at) VALUES(?,?,?)`,
		t, u.ID, now()); err != nil {
		return "", nil, err
	}
	return t, u, nil
}

// createSSOUser makes the local row for someone arriving on this team for the
// first time. The password is random and never leaves this function: an SSO
// account has no password to type, and leaving the column empty would make an
// empty string a valid one.
func (s *Store) createSSOUser(email, wantNick string) (*User, error) {
	nick := strings.TrimSpace(wantNick)
	if nick == "" {
		nick, _, _ = strings.Cut(email, "@")
	}
	nick = strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \t#@+", r) {
			return -1
		}
		return r
	}, nick)
	if nick == "" {
		nick = "member"
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret)), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	// The nick is display and has to be unique, so a clash gets a suffix rather
	// than a failed sign-in: the person did nothing wrong and has no way to
	// pick again from here.
	base, u := nick, &User{ID: id("u"), Email: email, Role: "member"}
	for i := 0; i < 50; i++ {
		u.Nick = nick
		_, err := s.db.Exec(
			`INSERT INTO users(id,nick,email,pass_hash,role,created_at,sso) VALUES(?,?,?,?,?,?,1)`,
			u.ID, u.Nick, email, string(h), u.Role, now())
		if err == nil {
			return u, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, err
		}
		nick = fmt.Sprintf("%s%d", base, i+2)
	}
	return nil, errors.New("could not find a free nick")
}
