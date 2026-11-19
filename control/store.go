package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

//go:embed schema.sql
var schemaSQL string

// The control plane's database.
//
// The control plane's own PostgreSQL database. It knows accounts, which teams
// they belong to, which shard each team lives on and whether it is paid up. It
// does not know a single message, channel or nick: those are in the team's own
// schema on its shard, and nothing here reads them.
//
// Keeping it off the chat path is deliberate. A team's server answers every
// request from its own schema; putting session lookup here would add a hop per
// request and a shared point of failure in front of every team.

type Store struct{ db *sql.DB }

func OpenStore(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func id(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func token() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	ErrNotFound = errors.New("not found")
	ErrTaken    = errors.New("already taken")
)

// ---- accounts ----

type Account struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
}

func normaliseEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Signup makes an account, or takes over an UNCONFIRMED one with the same
// address: nobody has proved that mailbox yet, so the earlier registration
// holds nothing, and the code mailed next decides who keeps it. A confirmed
// account is never touched (ErrTaken).
//
// One bcrypt on every path, and the takeover (password, pending code, every
// session) is one transaction on the locked row, so an old session cannot
// confirm the address halfway through it.
func (s *Store) Signup(email, password string) (*Account, error) {
	email = normaliseEmail(email)
	if !strings.Contains(email, "@") {
		return nil, errors.New("that is not an email address")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a := &Account{ID: id("a"), Email: email}
	res, err := tx.Exec(`INSERT INTO accounts(id,email,pass_hash) VALUES($1,$2,$3) ON CONFLICT(email) DO NOTHING`,
		a.ID, a.Email, string(h))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var verified bool
		if err := tx.QueryRow(`SELECT id,verified FROM accounts WHERE email=$1 FOR UPDATE`, email).
			Scan(&a.ID, &verified); err != nil {
			return nil, err
		}
		if verified {
			return nil, ErrTaken
		}
		if _, err := tx.Exec(`UPDATE accounts SET pass_hash=$1, verify_digest='', session_gen=session_gen+1
		  WHERE id=$2`, string(h), a.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM account_sessions WHERE account_id=$1`, a.ID); err != nil {
			return nil, err
		}
	}
	return a, tx.Commit()
}

// dummyHash is compared against when there is no account, so a login for a
// missing address costs what a wrong password does.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("no account has this password"), bcrypt.DefaultCost)

func (s *Store) Login(email, password string) (string, *Account, error) {
	email = normaliseEmail(email)
	var aid, hash string
	var verified bool
	var gen int64
	err := s.db.QueryRow(`SELECT id,pass_hash,verified,session_gen FROM accounts WHERE email=$1`, email).
		Scan(&aid, &hash, &verified, &gen)
	if err != nil {
		// Same bcrypt work as a real account, so the answer's timing does not
		// say which addresses have signed up either.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	}
	// One answer for "no such account" and "wrong password": telling them apart
	// turns this into a free check for which addresses have signed up.
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", nil, errors.New("wrong email or password")
	}
	t := token()
	// The generation read WITH the password: if the account is taken over
	// before this lands, the session is already stale (AccountByToken).
	if _, err := s.db.Exec(`INSERT INTO account_sessions(token,account_id,gen) VALUES($1,$2,$3)`, t, aid, gen); err != nil {
		return "", nil, err
	}
	return t, &Account{ID: aid, Email: email, Verified: verified}, nil
}

func (s *Store) AccountByToken(t string) (*Account, error) {
	if t == "" {
		return nil, ErrNotFound
	}
	a := &Account{}
	err := s.db.QueryRow(`
		SELECT a.id, a.email, a.verified FROM account_sessions s
		JOIN accounts a ON a.id = s.account_id
		WHERE s.token=$1 AND s.gen = a.session_gen
		  AND s.created_at > now() - interval '90 days'`, t).Scan(&a.ID, &a.Email, &a.Verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// NewVerifyCode replaces the account's code with a fresh one, good for a day,
// and returns it. Only its digest is stored: for that day it is a credential.
func (s *Store) NewVerifyCode(accountID string) (string, error) {
	code := token()[:12]
	_, err := s.db.Exec(`UPDATE accounts SET verify_digest=$1, verify_expires=now() + interval '1 day'
	  WHERE id=$2`, digest(code), accountID)
	return code, err
}

// Verify proves the address when the code matches and has not expired.
func (s *Store) Verify(accountID, code string) error {
	res, err := s.db.Exec(`UPDATE accounts SET verified=true, verify_digest=''
	  WHERE id=$1 AND verify_digest=$2 AND verify_digest<>'' AND verify_expires > now()`,
		accountID, digest(strings.TrimSpace(code)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that code is wrong or has expired")
	}
	return nil
}

func digest(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Logout(t string) { s.db.Exec(`DELETE FROM account_sessions WHERE token=$1`, t) }

// ---- shards ----

type Shard struct {
	Name      string `json:"name"`
	Pool      string `json:"pool"`
	BaseURL   string `json:"base_url"`
	Secret    string `json:"-"`
	Capacity  int    `json:"capacity"`
	Accepting bool   `json:"accepting"`
	Teams     int    `json:"teams"`
}

func (s *Store) AddShard(sh Shard) error {
	_, err := s.db.Exec(`
		INSERT INTO shards(name,pool,base_url,secret,capacity,accepting)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(name) DO UPDATE SET
		  pool=EXCLUDED.pool, base_url=EXCLUDED.base_url, secret=EXCLUDED.secret,
		  capacity=EXCLUDED.capacity, accepting=EXCLUDED.accepting`,
		sh.Name, sh.Pool, sh.BaseURL, sh.Secret, sh.Capacity, sh.Accepting)
	return err
}

func (s *Store) Shards() ([]Shard, error) {
	rows, err := s.db.Query(`
		SELECT s.name, s.pool, s.base_url, s.capacity, s.accepting, COUNT(v.slug)
		FROM shards s LEFT JOIN servers v ON v.shard = s.name
		GROUP BY s.name, s.pool, s.base_url, s.capacity, s.accepting
		ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shard
	for rows.Next() {
		var sh Shard
		if err := rows.Scan(&sh.Name, &sh.Pool, &sh.BaseURL, &sh.Capacity, &sh.Accepting, &sh.Teams); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// Place picks the shard a new team goes on: the emptiest one in the pool that
// still has room. That is the whole scheduler — a row and an ORDER BY, not a
// service. A dedicated customer is a pool of their own with capacity 1, which
// needs no special case anywhere.
func (s *Store) Place(pool string) (*Shard, error) { return place(s.db, pool) }

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func place(q querier, pool string) (*Shard, error) {
	sh := &Shard{}
	err := q.QueryRow(`
		SELECT s.name, s.base_url, s.secret
		FROM shards s LEFT JOIN servers v ON v.shard = s.name
		WHERE s.accepting AND s.pool = $1
		GROUP BY s.name, s.base_url, s.secret, s.capacity
		HAVING COUNT(v.slug) < s.capacity
		ORDER BY COUNT(v.slug), s.name
		LIMIT 1`, pool).Scan(&sh.Name, &sh.BaseURL, &sh.Secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no shard in pool %q has room", pool)
	}
	return sh, err
}

func (s *Store) ShardFor(slug string) (*Shard, error) {
	sh := &Shard{}
	err := s.db.QueryRow(`
		SELECT s.name, s.base_url, s.secret FROM servers v
		JOIN shards s ON s.name = v.shard WHERE v.slug=$1`, slug).
		Scan(&sh.Name, &sh.BaseURL, &sh.Secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sh, err
}

// ---- servers (teams) ----

type Server struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Shard  string `json:"-"`
	Plan   string `json:"plan"`
	Status string `json:"status"`
	// Nick and Role describe the CALLER's relationship to this team, so they
	// are only set by the queries that ask on behalf of an account.
	Nick string `json:"nick,omitempty"`
	Role string `json:"role,omitempty"`
}

// A team whose subscription lapsed is read-only, and a suspended one is not
// reachable at all. The shard stays dumb about money: this is the only place
// that knows, and it decides by refusing to mint an assertion.
const (
	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusSuspended = "suspended"
)

// PlaceAndCreate picks a shard with room and records the team on it as one
// step. The pool's advisory lock serialises it: two creates cannot both see
// the last free slot and overfill the shard.
var ErrTooManyTeams = errors.New("this account has started as many teams as it may")

func (s *Store) PlaceAndCreate(pool, slug, name, host, createdBy string, maxTeams int) (*Server, *Shard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('place:' || $1))`, pool); err != nil {
		return nil, nil, err
	}
	// A taken address is the caller's problem (409) before a full pool is
	// ours (503).
	var taken bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM servers WHERE slug=$1 OR host=$2)`, slug, host).Scan(&taken); err != nil {
		return nil, nil, err
	}
	if taken {
		return nil, nil, ErrTaken
	}
	// Counted under the lock too, so a burst of creates cannot all pass it.
	var started int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM servers WHERE created_by=$1`, createdBy).Scan(&started); err != nil {
		return nil, nil, err
	}
	if started >= maxTeams {
		return nil, nil, ErrTooManyTeams
	}
	sh, err := place(tx, pool)
	if err != nil {
		return nil, nil, err
	}
	v, err := createServer(tx, slug, name, host, sh.Name, pool, createdBy)
	if err != nil {
		return nil, nil, err
	}
	return v, sh, tx.Commit()
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func (s *Store) CreateServer(slug, name, host, shard, plan, createdBy string) (*Server, error) {
	return createServer(s.db, slug, name, host, shard, plan, createdBy)
}

func createServer(db execer, slug, name, host, shard, plan, createdBy string) (*Server, error) {
	v := &Server{Slug: slug, Name: name, Host: host, Shard: shard, Plan: plan, Status: StatusActive}
	_, err := db.Exec(`
		INSERT INTO servers(slug,name,host,shard,plan,status,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		v.Slug, v.Name, v.Host, v.Shard, v.Plan, v.Status, createdBy)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrTaken
		}
		return nil, err
	}
	return v, nil
}

// StartedBy returns a team this account created, so its creator's retry of
// the same create finishes or repeats it (provisioning and membership are both
// idempotent) instead of answering "taken": a creation that failed halfway,
// or one that finished and lost its answer. Anyone else is ErrTaken.
func (s *Store) StartedBy(slug, accountID string) (*Server, error) {
	v := &Server{}
	err := s.db.QueryRow(`
		SELECT slug,name,host,shard,plan,status FROM servers v
		WHERE slug=$1 AND created_by=$2 AND created_by<>''`, slug, accountID).
		Scan(&v.Slug, &v.Name, &v.Host, &v.Shard, &v.Plan, &v.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaken
	}
	return v, err
}

func (s *Store) ServersFor(accountID string) ([]Server, error) {
	rows, err := s.db.Query(`
		SELECT v.slug, v.name, v.host, v.plan, v.status, m.nick, m.role
		FROM memberships m JOIN servers v ON v.slug = m.slug
		WHERE m.account_id = $1 ORDER BY v.name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		var v Server
		if err := rows.Scan(&v.Slug, &v.Name, &v.Host, &v.Plan, &v.Status, &v.Nick, &v.Role); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Membership is the authority for single sign-on: an assertion is only ever
// minted for a team this account is actually in.
func (s *Store) Membership(accountID, slug string) (*Server, error) {
	v := &Server{}
	err := s.db.QueryRow(`
		SELECT v.slug, v.name, v.host, v.plan, v.status, m.nick, m.role
		FROM memberships m JOIN servers v ON v.slug = m.slug
		WHERE m.account_id=$1 AND m.slug=$2`, accountID, slug).
		Scan(&v.Slug, &v.Name, &v.Host, &v.Plan, &v.Status, &v.Nick, &v.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *Store) AddMember(accountID, slug, nick, role string) error {
	_, err := s.db.Exec(`
		INSERT INTO memberships(account_id,slug,nick,role) VALUES($1,$2,$3,$4)
		ON CONFLICT(account_id,slug) DO UPDATE SET nick=EXCLUDED.nick`,
		accountID, slug, nick, role)
	return err
}

func (s *Store) AccountByEmail(email string) (*Account, error) {
	a := &Account{}
	err := s.db.QueryRow(`SELECT id,email,verified FROM accounts WHERE email=$1`, normaliseEmail(email)).
		Scan(&a.ID, &a.Email, &a.Verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// SetStatus is where billing touches the product. Whatever ends up driving it —
// a payment provider's webhook, an invoice run, an admin — writes here and
// nothing else in Gliph has to know how it was decided.
func (s *Store) SetStatus(slug, status, billingRef string) (int64, error) {
	var v int64
	err := s.db.QueryRow(`UPDATE servers SET status=$1, billing_ref=$2, shard_synced=false,
	  status_version=status_version+1 WHERE slug=$3 RETURNING status_version`,
		status, billingRef, slug).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}

// MarkSynced records that the shard has this version of the status. A change
// that landed in between has a newer version, stays unsynced and goes next.
func (s *Store) MarkSynced(slug string, version int64) {
	s.db.Exec(`UPDATE servers SET shard_synced=true WHERE slug=$1 AND status_version=$2`, slug, version)
}

type pendingStatus struct {
	Status  string
	Version int64
}

// Unsynced lists the teams whose shard has not confirmed their status.
func (s *Store) Unsynced() (map[string]pendingStatus, error) {
	rows, err := s.db.Query(`SELECT slug,status,status_version FROM servers WHERE NOT shard_synced`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]pendingStatus{}
	for rows.Next() {
		var slug string
		var p pendingStatus
		if err := rows.Scan(&slug, &p.Status, &p.Version); err != nil {
			return nil, err
		}
		out[slug] = p
	}
	return out, rows.Err()
}
