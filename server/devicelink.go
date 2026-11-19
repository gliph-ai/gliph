package main

// "Link this device to my account" — an OAuth-style loopback + PKCE flow, so a
// desktop or phone client never has to ask for a password.
//
// WHY NOT JUST ASK FOR THE PASSWORD. Because every client that does becomes a
// place a password is typed, held in memory, and possibly logged — and because
// gliph's password is the thing an attacker most wants: it is on the road to
// the user's keys. A native app that never sees it cannot leak it. All the
// password, 2FA and SSO logic stays on the one surface built for it (the web
// client), and what the app ends up holding is a session it can be signed out
// of on its own, without touching the others.
//
// THE FLOW, and what each piece is actually defending against:
//
//	app                          browser (web client)          server
//	 │ make verifier + challenge
//	 │ bind 127.0.0.1:<port>/cb
//	 │ open ────────────────────▶ /link?…&code_challenge=…
//	 │                            user is signed in, approves
//	 │                            POST auth/device-link/authorize ─▶ mint code
//	 │ ◀── 302 127.0.0.1/cb?code&state ──                          (pending)
//	 │ check state
//	 │ POST auth/device-link/token {code, code_verifier} ─────────▶ verify, mint
//	 │ ◀── {token, user} ─────────────────────────────────────────  session
//
//   - PKCE (S256) binds the code to the app that started the flow. Another
//     local process can race for the loopback port and catch the code, but it
//     cannot redeem it: the verifier never left the app that made it. This is
//     the whole reason the flow is safe on a shared machine.
//   - `state` is the app's CSRF check that the callback it caught belongs to
//     the flow it started.
//   - the redirect target is validated LOOPBACK-ONLY, server side. Without that,
//     a crafted /link URL could ship someone's fresh code to any host on the
//     internet.
//   - the code is single-use and short-lived, and is stored as a digest. A read
//     of the database does not hand anyone a usable code.
//
// WHAT THIS DOES NOT DO. A linked device gets a session, not trust. Before it
// can decrypt anything, an approved device of the account has to sign it with
// /approve (crypto.go, docs/protocol.md #Device trust). That runs on the live
// socket rather than here, so it covers a browser sign-in as well as a linked
// app.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A code is worth minutes, not hours: the human is standing right there.
const deviceCodeTTL = 5 * time.Minute

var devicePlatforms = map[string]bool{"desktop": true, "ios": true, "android": true}

// ---- store ----

type DeviceLinkRequest struct {
	Challenge string
	Redirect  string
	Name      string
	Platform  string
	UserID    string
	ExpiresAt time.Time
}

// digest is what actually goes in the table. Codes are bearer secrets for their
// five minutes, and a bearer secret is stored the way a password is.
func codeDigest(code string) string {
	sum := sha256.Sum256([]byte(code))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Store) CreateDeviceLink(uid, challenge, redirect, name, platform string) (string, error) {
	code := token()
	_, err := s.db.Exec(`
		INSERT INTO device_links (id,code_digest,user_id,code_challenge,redirect_uri,device_name,platform,created_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id("dlr"), codeDigest(code), uid, challenge, redirect, name, platform,
		now(), time.Now().UTC().Add(deviceCodeTTL).Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return code, nil
}

var errBadCode = errors.New("code is invalid, used or expired")

// RedeemDeviceLink consumes a code and returns what it was issued for. It is
// the single place that decides a code is spent, and it deletes the row in the
// same transaction as reading it, so two racing redemptions cannot both win.
func (s *Store) RedeemDeviceLink(code string) (*DeviceLinkRequest, error) {
	if code == "" {
		return nil, errBadCode
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var rowID, digest, expires string
	var r DeviceLinkRequest
	err = tx.QueryRow(`
		SELECT id,code_digest,user_id,code_challenge,redirect_uri,device_name,platform,expires_at
		FROM device_links WHERE code_digest=?`, codeDigest(code)).
		Scan(&rowID, &digest, &r.UserID, &r.Challenge, &r.Redirect, &r.Name, &r.Platform, &expires)
	if err != nil {
		return nil, errBadCode
	}
	// The lookup was already by digest, so this comparison is belt and braces —
	// but it is the cheap kind, and it keeps the constant-time habit in the one
	// file where a timing leak would matter.
	if subtle.ConstantTimeCompare([]byte(digest), []byte(codeDigest(code))) != 1 {
		return nil, errBadCode
	}
	if _, err := tx.Exec(`DELETE FROM device_links WHERE id=?`, rowID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil || time.Now().UTC().After(exp) {
		return nil, errBadCode
	}
	return &r, nil
}

// PruneDeviceLinks drops codes nobody redeemed. They are already refused once
// expired; this is so the table does not grow forever.
func (s *Store) PruneDeviceLinks() {
	s.db.Exec(`DELETE FROM device_links WHERE expires_at < ?`, now())
}

// NewDeviceSession mints the session a linked device holds. It is an ordinary
// session — the same thing a browser login produces — but named, so the account
// can list its devices and sign out exactly one of them.
func (s *Store) NewDeviceSession(uid, name, platform string) (string, error) {
	t := token()
	_, err := s.db.Exec(
		`INSERT INTO sessions(token,user_id,created_at,device_name,platform) VALUES(?,?,?,?,?)`,
		t, uid, now(), name, platform)
	return t, err
}

type LinkedDevice struct {
	ID       string `json:"id"` // an opaque handle for revocation, never the token
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Created  string `json:"created_at"`
	Current  bool   `json:"current"`
}

func (s *Store) LinkedDevices(uid, currentToken string) ([]LinkedDevice, error) {
	rows, err := s.db.Query(
		`SELECT token,device_name,platform,created_at FROM sessions
		 WHERE user_id=? AND device_name <> '' ORDER BY created_at DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LinkedDevice{}
	for rows.Next() {
		var tok string
		var d LinkedDevice
		if err := rows.Scan(&tok, &d.Name, &d.Platform, &d.Created); err != nil {
			return nil, err
		}
		// The handle is a digest of the token, not the token: listing your own
		// devices must not hand a stolen session the credentials of the others.
		d.ID = codeDigest(tok)[:16]
		d.Current = tok == currentToken
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) RevokeLinkedDevice(uid, handle string) error {
	rows, err := s.db.Query(`SELECT token FROM sessions WHERE user_id=? AND device_name <> ''`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	var match string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			return err
		}
		if codeDigest(tok)[:16] == handle {
			match = tok
		}
	}
	if match == "" {
		return ErrNotFound
	}
	_, err = s.db.Exec(`DELETE FROM sessions WHERE token=? AND user_id=?`, match, uid)
	return err
}

// ---- validation ----

// loopbackRedirect enforces that a code can only ever be delivered back to
// something running on this machine. Without this check, a crafted /link URL is
// a way to have a signed-in user hand their fresh code to any host on the
// internet — the flow's one genuinely dangerous failure mode.
func loopbackRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("redirect_uri is not a URL")
	}
	if u.Scheme != "http" {
		return errors.New("redirect_uri must be http on loopback")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		// Deliberately no "localhost": it resolves through the host's name
		// service, and what that returns is not this code's to assume.
		return errors.New("redirect_uri must be a loopback IP (127.0.0.1 or ::1)")
	}
	if u.Path == "" || strings.Contains(raw, "..") {
		return errors.New("redirect_uri needs a path")
	}
	return nil
}

// verifierMatches is the PKCE check: BASE64URL(SHA256(verifier)) == challenge.
func verifierMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// ---- routes ----

func (s *Server) deviceLinkRoutes(mux *http.ServeMux) {
	// Approve a link. Authenticated as the signed-in user in the browser: this
	// is the step where a human says yes.
	mux.HandleFunc("POST /api/auth/device-link/authorize", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		var d struct {
			DeviceName          string `json:"device_name"`
			Platform            string `json:"platform"`
			RedirectURI         string `json:"redirect_uri"`
			CodeChallenge       string `json:"code_challenge"`
			CodeChallengeMethod string `json:"code_challenge_method"`
		}
		json.NewDecoder(r.Body).Decode(&d)

		// S256 only. `plain` puts the verifier itself in a URL the browser and
		// its history can see, which is the exact thing PKCE exists to avoid.
		if d.CodeChallengeMethod != "S256" {
			fail(w, 400, "code_challenge_method must be S256")
			return
		}
		if len(d.CodeChallenge) < 32 || len(d.CodeChallenge) > 128 {
			fail(w, 400, "bad code_challenge")
			return
		}
		// Substrate (docs/substrate.md) is the one first-party web client: its
		// code goes to exactly the callback this process was configured with,
		// never a URL the request names. Everything else stays loopback-only.
		if d.Platform == "substrate" {
			if s.SubstrateCallback == "" || d.RedirectURI != s.SubstrateCallback {
				fail(w, 400, "redirect_uri is not this server's Substrate callback")
				return
			}
		} else if !devicePlatforms[d.Platform] {
			fail(w, 400, "platform must be desktop, ios or android")
			return
		} else if err := loopbackRedirect(d.RedirectURI); err != nil {
			fail(w, 400, err.Error())
			return
		}
		name := strings.TrimSpace(d.DeviceName)
		if name == "" {
			name = d.Platform
		}
		if len(name) > 64 {
			name = name[:64]
		}

		code, err := s.St.CreateDeviceLink(u.ID, d.CodeChallenge, d.RedirectURI, name, d.Platform)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.St.PruneDeviceLinks()
		writeJSON(w, 200, map[string]string{"code": code, "redirect_uri": d.RedirectURI})
	})

	// Redeem a code for a session. PUBLIC by design: the holder of the verifier
	// matching the approved challenge IS the authorized client, and requiring
	// any other credential here would defeat the point of the flow.
	mux.HandleFunc("POST /api/auth/device-link/token", func(w http.ResponseWriter, r *http.Request) {
		var d struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
			RedirectURI  string `json:"redirect_uri"`
		}
		json.NewDecoder(r.Body).Decode(&d)

		// Rate-limited per IP: this endpoint takes an unauthenticated secret, so
		// it is the one place someone could sit and guess.
		if !s.Limits.LoginIP.Allow(clientIP(r)) {
			fail(w, 429, "too many attempts")
			return
		}

		req, err := s.St.RedeemDeviceLink(d.Code)
		if err != nil {
			fail(w, 400, errBadCode.Error())
			return
		}
		// From here the code is already spent, whatever happens next. A failed
		// verifier does not get a second try with the same code.
		if !verifierMatches(d.CodeVerifier, req.Challenge) {
			fail(w, 400, errBadCode.Error())
			return
		}
		if d.RedirectURI != req.Redirect {
			fail(w, 400, errBadCode.Error())
			return
		}
		u, err := s.St.UserByID(req.UserID)
		if err != nil {
			fail(w, 400, errBadCode.Error())
			return
		}
		tok, err := s.St.NewDeviceSession(req.UserID, req.Name, req.Platform)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		resp := map[string]any{"token": tok, "user": u}
		// Substrate signs people in by email, and its approval page says it
		// gets the nick and the email. No other client is handed the address.
		if req.Platform == "substrate" {
			resp["email"] = u.Email
		}
		writeJSON(w, 200, resp)
	})

	// The account's own list of linked devices, and the ability to sign one out.
	mux.HandleFunc("GET /api/auth/devices", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		list, err := s.St.LinkedDevices(u.ID, bearer(r))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, list)
	})

	mux.HandleFunc("DELETE /api/auth/devices/{handle}", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		if err := s.St.RevokeLinkedDevice(u.ID, r.PathValue("handle")); err != nil {
			fail(w, 404, "no such device")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

func bearer(r *http.Request) string {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if t == "" {
		t = r.URL.Query().Get("token")
	}
	return t
}
