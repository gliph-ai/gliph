package main

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// readPool is how many connections serve reads. WAL gives every reader its own
// snapshot, so they neither block each other nor the writer; the number is a
// cap on concurrent catch-up work, not a correctness knob.
const readPool = 8

// pgReadPool is the reader cap on PostgreSQL, where connections are shared
// with every other team on the database (see OpenStore).
const pgReadPool = 4

type Store struct {
	// db is the WRITER: exactly one connection (see OpenStore). Everything that
	// mutates goes here, and so does any read that has not been moved to `r`.
	db *DB
	// r is the READER pool, opened query_only so a write routed here fails loudly
	// instead of intermittently under load. See the comment in OpenStore.
	r *DB
	// inviteOnly is set for a hosted team (shard.go): its first account comes
	// in through the owner invite like everyone after it. Anyone can reach a
	// new team's address, so "first to register is admin" would hand the team
	// to whoever got there before its owner. A self-host keeps that rule: its
	// operator is the first to reach a server nobody else knows about.
	inviteOnly bool
}

// OpenStore opens a SQLite file, or PostgreSQL when given a postgres:// URL.
func OpenStore(path string) (*Store, error) {
	pg := isPostgres(path)
	// synchronous=NORMAL, not the WAL default of FULL: an fsync per message caps
	// throughput on a real disk, and under WAL the trade is losing the last few
	// commits on a power cut, never a corrupt database.
	const dsn = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	driver, wdsn, rdsn := "sqlite", path+dsn, path+dsn+"&_pragma=query_only(1)"
	if pg {
		// The reader pool's guard against a misrouted write, PostgreSQL's way.
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		driver, wdsn, rdsn = "pgx", path, path+sep+"default_transaction_read_only=on"
	}
	sdb, err := sql.Open(driver, wdsn)
	if err != nil {
		return nil, err
	}
	db := &DB{sdb, pg}
	// ponytail: single writer connection avoids SQLITE_BUSY without a write queue.
	//
	// It has to stay one. Append opens a deferred transaction, reads MAX(seq) and
	// then upgrades to a write — the case SQLite answers with SQLITE_BUSY_SNAPSHOT
	// and does NOT retry, busy_timeout or not. Concurrency comes from the reader
	// pool below instead, where it is free.
	//
	// On PostgreSQL the same one connection is what keeps Append's MAX(seq)+1 and
	// Register's first-account count race-free inside this process. Two
	// processes overlap only for the seconds of a deploy; a seq collision there
	// is refused by events_chan_seq, not written twice.
	db.SetMaxOpenConns(1)
	if _, err := db.DB.Exec(ddl(pg, schemaSQL)); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// schema.sql is CREATE-only, so columns added to an existing table need an
	// explicit ALTER. Each is idempotent: a duplicate-column error means the
	// migration already ran.
	for _, mig := range []string{
		`ALTER TABLE events ADD COLUMN enc INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0`,
		// A session minted by the device-link flow carries the device's name and
		// platform, so an account can list what is signed in and revoke one of
		// them. A browser login leaves both empty.
		`ALTER TABLE sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN platform TEXT NOT NULL DEFAULT ''`,
		// owner_id and user_id below are the same fix in two places: a nick is
		// DISPLAY, never identity. Both tables used to key an authorisation
		// decision on one, and /nick changes a nick — so the decision changed
		// with it. The nick columns stay, because both are also rendered.
		`ALTER TABLE uploads ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bans ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`,
		// The two timestamps the missed-message digest runs on (email.go).
		// last_seen is the last moment we know the account was connected;
		// last_digest is when we last mailed it. Both empty on an account that
		// has never connected or never been mailed, and an empty stamp sorts
		// below every RFC3339 one, which is what the digest's queries rely on.
		`ALTER TABLE users ADD COLUMN last_seen TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN last_digest TEXT NOT NULL DEFAULT ''`,
		// 1 for an account single sign-on created (sso.go). Only those are
		// ever signed in to by an asserted email.
		`ALTER TABLE users ADD COLUMN sso INTEGER NOT NULL DEFAULT 0`,
		// Device signing (crypto.go): the account master key, each device's
		// signature and the sealed master key its approver left for it, and
		// the key epoch each member joined at for sealed history.
		`ALTER TABLE users ADD COLUMN master_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN sig TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN sealed_master TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE members ADD COLUMN joined_epoch INTEGER NOT NULL DEFAULT 0`,
		// Webhooks in and out (webhooks.go): an incoming hook posts under a
		// name, an outgoing one delivers to a URL. A git hook has neither.
		`ALTER TABLE hooks ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hooks ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.DB.Exec(ddl(pg, mig)); err != nil && !dupColumn(err) {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// read_seq is how far the USER has read; cursor above it is how far we have
	// DELIVERED. They were one number until read state had to survive moving
	// between devices, and conflating them made every badge wrong in one
	// direction or the other.
	//
	// The seed runs exactly once, on the boot that adds the column: a successful
	// ALTER is the flag. Re-running it on every start would overwrite real read
	// marks with the delivery cursor, which is the bug this column exists to fix.
	if _, err := db.DB.Exec(ddl(pg, `ALTER TABLE members ADD COLUMN read_seq INTEGER NOT NULL DEFAULT 0`)); err == nil {
		// Existing members should not open the app to a wall of unread they had
		// already read, so the old delivery cursor is the best guess available.
		if _, err := db.Exec(`UPDATE members SET read_seq=cursor`); err != nil {
			return nil, fmt.Errorf("migrate: seed read_seq: %w", err)
		}
	} else if !dupColumn(err) {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Devices registered before signing existed were already trusted by their
	// peers. The boot that adds the column marks them, once, so the account's
	// first signer may vouch for them and for nothing that arrives later.
	if _, err := db.DB.Exec(ddl(pg, `ALTER TABLE devices ADD COLUMN legacy INTEGER NOT NULL DEFAULT 0`)); err == nil {
		if _, err := db.Exec(`UPDATE devices SET legacy=1`); err != nil {
			return nil, fmt.Errorf("migrate: seed legacy devices: %w", err)
		}
	} else if !dupColumn(err) {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Reads run on their own pool. On one shared connection a single slow query —
	// the admin console counting events, a blob scanning a channel — stalls every
	// message on the server behind it. query_only turns a misrouted write into an
	// immediate error rather than a rare SQLITE_BUSY under load.
	sr, err := sql.Open(driver, rdsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	r := &DB{sr, pg}
	r.SetMaxOpenConns(readPool)
	if pg {
		// PostgreSQL connections are a shared budget (max_connections, ~100
		// on the dev plan), and a shard opens one Store per team. A team that
		// is quiet holds none: idle connections close after a minute, and the
		// read pool is half SQLite's. Through a pooler these are client
		// connections and cost little; without one they are the ceiling on
		// teams per shard.
		r.SetMaxOpenConns(pgReadPool)
		for _, d := range []*DB{db, r} {
			d.SetMaxIdleConns(1)
			d.SetConnMaxIdleTime(time.Minute)
		}
	}
	return &Store{db: db, r: r}, nil
}

func (s *Store) Close() error {
	s.r.Close()
	return s.db.Close()
}

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

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---- users & auth ----

type User struct {
	ID    string `json:"id"`
	Nick  string `json:"nick"`
	Email string `json:"-"`
	Role  string `json:"role"`
	Away  string `json:"away,omitempty"`
	Susp  bool   `json:"-"`
}

var ErrNotFound = errors.New("not found")

// Invite failures are told apart from account failures so the HTTP layer can
// answer 403 rather than 400, the way it always has.
var (
	ErrBadInvite  = errors.New("invalid invite")
	ErrUsedInvite = errors.New("invite already used")
)

// Register creates an account and consumes its invite as one transaction.
//
// Both halves of that matter. The invite used to be marked used BEFORE the
// account was created, so a mistyped password — or a nick already taken — spent
// it and locked the person out for good; on a closed server the invite is the
// only way in, and only an admin can mint another. And "is this invite free"
// asked in one statement then claimed in the next lets two people redeem the
// same invite and both win.
//
// The bcrypt hash is computed BEFORE the transaction opens. It costs ~100ms, and
// this store has a single writer connection (see OpenStore) — hashing inside
// would stall every other write on the server for that long.
func (s *Store) Register(nick, email, pass, invite string) (*User, error) {
	nick = strings.TrimSpace(nick)
	if nick == "" || strings.ContainsAny(nick, " \t#@+") {
		return nil, errors.New("invalid nick")
	}
	if len(pass) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	u := &User{ID: id("u"), Nick: nick, Email: email, Role: "member"}

	// The first account on an empty server owns it. Counting inside the
	// transaction is what stops two simultaneous first registrations from both
	// coming out as admin.
	var users int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return nil, err
	}
	if users == 0 && !s.inviteOnly {
		u.Role = "admin"
	} else {
		// Check and claim in ONE statement: the WHERE is the check, so a second
		// redeemer of the same invite matches no rows instead of also winning.
		res, err := tx.Exec(
			`UPDATE invites SET used_by=? WHERE token=? AND COALESCE(used_by,'')=''`, u.ID, invite)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Say which it was. Whoever is holding the token can already tell
			// these apart by trying it twice.
			var exists int
			tx.QueryRow(`SELECT COUNT(*) FROM invites WHERE token=?`, invite).Scan(&exists)
			if exists > 0 {
				return nil, ErrUsedInvite
			}
			return nil, ErrBadInvite
		}
		if err := tx.QueryRow(`SELECT role FROM invites WHERE token=?`, invite).Scan(&u.Role); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(`INSERT INTO users(id,nick,email,pass_hash,role,created_at) VALUES(?,?,?,?,?,?)`,
		u.ID, nick, email, string(h), u.Role, now()); err != nil {
		// Rolls back the invite claim with it — that is the whole point.
		return nil, errors.New("nick or email already taken")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) CreateUser(nick, email, pass, role string) (*User, error) {
	nick = strings.TrimSpace(nick)
	if nick == "" || strings.ContainsAny(nick, " \t#@+") {
		return nil, errors.New("invalid nick")
	}
	if len(pass) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{ID: id("u"), Nick: nick, Email: email, Role: role}
	_, err = s.db.Exec(`INSERT INTO users(id,nick,email,pass_hash,role,created_at) VALUES(?,?,?,?,?,?)`,
		u.ID, nick, email, string(h), role, now())
	if err != nil {
		return nil, fmt.Errorf("nick or email already taken")
	}
	return u, nil
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	u := &User{}
	var susp int
	err := row.Scan(&u.ID, &u.Nick, &u.Email, &u.Role, &u.Away, &susp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Susp = susp == 1
	return u, err
}

func (s *Store) UserByNick(nick string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,nick,email,role,away,suspended FROM users WHERE nick=?`, nick))
}

func (s *Store) UserByID(uid string) (*User, error) {
	// Paired with UserByToken on every authenticated request.
	return s.scanUser(s.r.QueryRow(`SELECT id,nick,email,role,away,suspended FROM users WHERE id=?`, uid))
}

func (s *Store) Login(nick, pass string) (string, *User, error) {
	var uid, hash string
	var susp int
	err := s.db.QueryRow(`SELECT id,pass_hash,suspended FROM users WHERE nick=? OR email=?`, nick, nick).
		Scan(&uid, &hash, &susp)
	if err != nil {
		return "", nil, errors.New("wrong nick or password")
	}
	if susp == 1 {
		return "", nil, errors.New("account suspended")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return "", nil, errors.New("wrong nick or password")
	}
	t := token()
	if _, err := s.db.Exec(`INSERT INTO sessions(token,user_id,created_at) VALUES(?,?,?)`, t, uid, now()); err != nil {
		return "", nil, err
	}
	u, err := s.UserByID(uid)
	return t, u, err
}

// sessionTTL is how long a token stays good. An ABSOLUTE cap, not a sliding
// window: refreshing on every request would be a write per request against the
// single writer connection (see OpenStore), which is the one thing this store
// cannot afford. Ninety days is well past any session someone still remembers
// opening, and the alternative — what this had before — was a token that never
// expired at all, so a copy lifted from localStorage once was good forever.
const sessionTTL = 90 * 24 * time.Hour

func (s *Store) UserByToken(t string) (*User, error) {
	var uid, created string
	// Every HTTP request and every socket connect lands here — the single
	// busiest read on the server.
	if err := s.r.QueryRow(`SELECT user_id,created_at FROM sessions WHERE token=?`, t).Scan(&uid, &created); err != nil {
		return nil, ErrNotFound
	}
	// Fail closed on an unparseable timestamp. Every row is written by now(),
	// so the only way to get here is a corrupted or hand-edited one, and an
	// auth check is not the place to give something the benefit of the doubt.
	if ts, err := time.Parse(time.RFC3339, created); err != nil || time.Since(ts) > sessionTTL {
		return nil, ErrNotFound
	}
	u, err := s.UserByID(uid)
	if err == nil && u.Susp {
		return nil, errors.New("account suspended")
	}
	return u, err
}

func (s *Store) Logout(t string) { s.db.Exec(`DELETE FROM sessions WHERE token=?`, t) }

// PruneSessions drops what UserByToken already refuses. Purely housekeeping —
// the expiry above is what makes a stale token useless; this is so the table
// does not keep a row for every browser tab this server has ever seen.
func (s *Store) PruneSessions() {
	cut := time.Now().UTC().Add(-sessionTTL).Format(time.RFC3339)
	s.db.Exec(`DELETE FROM sessions WHERE created_at < ?`, cut)
}

// SetPassword resets a password by nick. Used by the -passwd operator flag;
// every existing session stays valid, so a reset does not kick the user out.
func (s *Store) SetPassword(nick, pass string) error {
	if len(pass) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET pass_hash=? WHERE nick=?`, string(h), nick)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such user: %s", nick)
	}
	return nil
}

func (s *Store) SetAway(uid, text string) error {
	_, err := s.db.Exec(`UPDATE users SET away=? WHERE id=?`, text, uid)
	return err
}

func (s *Store) SetNick(uid, nick string) error {
	if strings.ContainsAny(nick, " \t#@+") || nick == "" {
		return errors.New("invalid nick")
	}
	_, err := s.db.Exec(`UPDATE users SET nick=? WHERE id=?`, nick, uid)
	if err != nil {
		return errors.New("nick already taken")
	}
	return nil
}

// ---- invites ----

func (s *Store) CreateInvite(role string) (string, error) {
	t := token()
	_, err := s.db.Exec(`INSERT INTO invites(token,role,created_at) VALUES(?,?,?)`, t, role, now())
	return t, err
}

// There is no UseInvite here on purpose. It used to check "is this invite
// free?" and claim it in two separate statements, which let two people redeem
// the same invite and both win. Register does it in one UPDATE ... WHERE
// (see above), and is the only path that redeems anything.

// ---- channels ----

type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic"`
	Modes   string `json:"modes"`
	Key     string `json:"-"`
	IsDM    bool   `json:"is_dm"`
	History string `json:"history"`
}

func (c *Channel) HasMode(m rune) bool { return strings.ContainsRune(c.Modes, m) }

func (s *Store) CreateChannel(name, topic string, isDM bool) (*Channel, error) {
	return s.CreateChannelWith(name, topic, isDM, "open")
}

// CreateChannelWith also sets the history policy: "open" (joiners read what
// was said before) or "sealed" (they start at their join). It is fixed here,
// at creation, and nothing changes it later.
func (s *Store) CreateChannelWith(name, topic string, isDM bool, history string) (*Channel, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	if name == "" || strings.ContainsAny(name, " \t,") {
		return nil, errors.New("invalid channel name")
	}
	if history != "open" && history != "sealed" {
		return nil, errors.New("history must be open or sealed")
	}
	c := &Channel{ID: id("c"), Name: name, Topic: topic, Modes: "nt", IsDM: isDM, History: history}
	dm := 0
	if isDM {
		dm = 1
		c.Modes = "i"
	}
	_, err := s.db.Exec(`INSERT INTO channels(id,name,topic,modes,is_dm,history,created_at) VALUES(?,?,?,?,?,?,?)`,
		c.ID, name, topic, c.Modes, dm, c.History, now())
	if err != nil {
		return nil, errors.New("channel already exists")
	}
	return c, nil
}

func (s *Store) scanChannel(row *sql.Row) (*Channel, error) {
	c := &Channel{}
	var dm int
	err := row.Scan(&c.ID, &c.Name, &c.Topic, &c.Modes, &c.Key, &dm, &c.History)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	c.IsDM = dm == 1
	return c, err
}

const chanCols = `id,name,topic,modes,chan_key,is_dm,history`

func (s *Store) ChannelByName(n string) (*Channel, error) {
	n = strings.TrimPrefix(n, "#")
	return s.scanChannel(s.db.QueryRow(`SELECT `+chanCols+` FROM channels WHERE name=?`, n))
}

func (s *Store) ChannelByID(cid string) (*Channel, error) {
	return s.scanChannel(s.db.QueryRow(`SELECT `+chanCols+` FROM channels WHERE id=?`, cid))
}

func (s *Store) SetTopic(cid, topic string) error {
	_, err := s.db.Exec(`UPDATE channels SET topic=? WHERE id=?`, topic, cid)
	return err
}

func (s *Store) SetModes(cid, modes string) error {
	_, err := s.db.Exec(`UPDATE channels SET modes=? WHERE id=?`, modes, cid)
	return err
}

func (s *Store) SetChannelKey(cid, key string) error {
	_, err := s.db.Exec(`UPDATE channels SET chan_key=? WHERE id=?`, key, cid)
	return err
}

// PublicChannels powers /list: public channels with member counts.
func (s *Store) PublicChannels() ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT c.id,c.name,c.topic,c.modes,
	  (SELECT COUNT(*) FROM members m WHERE m.channel_id=c.id)
	  FROM channels c WHERE c.is_dm=0 AND c.modes NOT LIKE '%i%' ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var cid, name, topic, modes string
		var n int
		if err := rows.Scan(&cid, &name, &topic, &modes, &n); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": cid, "name": name, "topic": topic, "modes": modes, "members": n})
	}
	return out, rows.Err()
}

// ---- channel invites (+i grants; survives a restart, spent by the join) ----

func (s *Store) AddChannelInvite(cid, uid, byNick string) error {
	_, err := s.db.Exec(`INSERT INTO channel_invites(channel_id,user_id,by_nick,created_at) VALUES(?,?,?,?)
	  ON CONFLICT(channel_id,user_id) DO NOTHING`, cid, uid, byNick, now())
	return err
}

func (s *Store) HasChannelInvite(cid, uid string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM channel_invites WHERE channel_id=? AND user_id=?`, cid, uid).Scan(&n)
	return n > 0
}

func (s *Store) DeleteChannelInvite(cid, uid string) error {
	_, err := s.db.Exec(`DELETE FROM channel_invites WHERE channel_id=? AND user_id=?`, cid, uid)
	return err
}

// ---- membership ----

func (s *Store) Join(cid, uid, role string) error {
	seq, _ := s.MaxSeq(cid)
	_, err := s.db.Exec(`INSERT INTO members(channel_id,user_id,role,joined_seq,joined_epoch) VALUES(?,?,?,?,?)
	  ON CONFLICT(channel_id,user_id) DO NOTHING`, cid, uid, role, seq, s.MaxEpoch(cid))
	return err
}

func (s *Store) Leave(cid, uid string) error {
	_, err := s.db.Exec(`DELETE FROM members WHERE channel_id=? AND user_id=?`, cid, uid)
	return err
}

func (s *Store) MemberRole(cid, uid string) (string, bool) {
	var r string
	err := s.db.QueryRow(`SELECT role FROM members WHERE channel_id=? AND user_id=?`, cid, uid).Scan(&r)
	if err != nil {
		return "", false
	}
	return r, true
}

func (s *Store) SetMemberRole(cid, uid, role string) error {
	res, err := s.db.Exec(`UPDATE members SET role=? WHERE channel_id=? AND user_id=?`, role, cid, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("not a member of this channel")
	}
	return nil
}

type Member struct {
	UserID string `json:"user_id"`
	Nick   string `json:"nick"`
	Role   string `json:"role"`
	Away   string `json:"away,omitempty"`
	Kind   string `json:"kind"` // user | agent | bot
	// Online is not stored: the server fills it from its socket registry when
	// it builds a channel view.
	Online bool `json:"online"`
	// Has a device no other device of the account approved. Peers seal it
	// nothing, and every client flags it in the member list.
	Unverified bool `json:"unverified,omitempty"`
}

func (s *Store) Members(cid string) ([]Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.nick,m.role,u.away,
	  EXISTS(SELECT 1 FROM devices d WHERE d.user_id=u.id AND d.sig='') FROM members m
	  JOIN users u ON u.id=m.user_id WHERE m.channel_id=?
	  ORDER BY CASE m.role WHEN 'op' THEN 0 WHEN 'voice' THEN 1 ELSE 2 END, u.nick`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Nick, &m.Role, &m.Away, &m.Unverified); err != nil {
			return nil, err
		}
		m.Kind = "user"
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// plugins joined to this channel are members too, and say so in the list
	prows, err := s.db.Query(`SELECT p.id,p.display,p.kind FROM plugin_channels pc
	  JOIN plugins p ON p.id=pc.plugin_id WHERE pc.channel_id=? ORDER BY p.display`, cid)
	if err != nil {
		return out, nil
	}
	defer prows.Close()
	for prows.Next() {
		var pid, disp, kind string
		if err := prows.Scan(&pid, &disp, &kind); err == nil {
			out = append(out, Member{UserID: pid, Nick: disp, Role: "voice", Kind: kind})
		}
	}
	return out, nil
}

func (s *Store) ChannelsFor(uid string) ([]*Channel, error) {
	rows, err := s.r.Query(`SELECT c.`+strings.ReplaceAll(chanCols, ",", ",c.")+` FROM channels c
	  JOIN members m ON m.channel_id=c.id WHERE m.user_id=? ORDER BY c.is_dm, c.name`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Channel{}
	for rows.Next() {
		c := &Channel{}
		var dm int
		if err := rows.Scan(&c.ID, &c.Name, &c.Topic, &c.Modes, &c.Key, &dm, &c.History); err != nil {
			return nil, err
		}
		c.IsDM = dm == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// MemberIDs returns user ids for fan-out.
func (s *Store) MemberIDs(cid string) []string {
	rows, err := s.db.Query(`SELECT user_id FROM members WHERE channel_id=?`, cid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			out = append(out, u)
		}
	}
	return out
}

// ---- bans ----

// Ban records a ban by BOTH nick and (when the nick resolves to an account)
// user id. The nick alone is what this used to store, and /nick made that a
// one-word ban evasion: rename, rejoin, done. The id is what actually follows
// someone; the nick is kept because /bans renders it, and because banning a
// nick nobody has registered yet is a real thing to want.
func (s *Store) Ban(cid, nick, uid, reason, by string) error {
	_, err := s.db.Exec(`INSERT INTO bans(channel_id,nick,user_id,reason,by_nick,created_at) VALUES(?,?,?,?,?,?)
	  ON CONFLICT(channel_id,nick) DO UPDATE SET reason=excluded.reason, user_id=excluded.user_id`,
		cid, nick, uid, reason, by, now())
	return err
}

// Unban lifts by nick — the handle an op typed and the one /bans shows. The row
// carries the id, so removing it removes both halves.
func (s *Store) Unban(cid, nick string) error {
	_, err := s.db.Exec(`DELETE FROM bans WHERE channel_id=? AND nick=?`, cid, nick)
	return err
}

// IsBanned matches either half: the id catches someone who renamed, the nick
// catches a ban laid down before that account existed.
func (s *Store) IsBanned(cid, nick, uid string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM bans WHERE channel_id=? AND (nick=? OR (user_id<>'' AND user_id=?))`,
		cid, nick, uid).Scan(&n)
	return n > 0
}

func (s *Store) Bans(cid string) []map[string]string {
	rows, err := s.db.Query(`SELECT nick,reason,by_nick FROM bans WHERE channel_id=?`, cid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var n, r, b string
		if rows.Scan(&n, &r, &b) == nil {
			out = append(out, map[string]string{"nick": n, "reason": r, "by": b})
		}
	}
	return out
}

// ---- events ----

type Event struct {
	Seq     int64  `json:"seq"`
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	TS      string `json:"ts"`
	// Body is plaintext for server-generated events (joins, modes, topics) and
	// opaque base64 ciphertext when Enc is set. The server never interprets it
	// in the encrypted case.
	Body  string         `json:"body"`
	Enc   bool           `json:"enc,omitempty"`
	Epoch int64          `json:"epoch,omitempty"`
	Meta  map[string]any `json:"meta,omitempty"`

	// actorID is who Actor IS, when the caller knows. Never serialised and
	// never stored — the wire and the log speak nicks. It exists so "do not
	// wake someone for their own message" survives a rename, which comparing
	// Actor could not. Empty is fine: notifyOffline resolves it once.
	actorID string
}

func (s *Store) MaxSeq(cid string) (int64, error) {
	var n sql.NullInt64
	err := s.r.QueryRow(`SELECT MAX(seq) FROM events WHERE channel_id=?`, cid).Scan(&n)
	return n.Int64, err
}

// Append writes one event with the next per-channel seq. A repeated nonce
// returns the already-stored event instead of a duplicate.
func (s *Store) Append(e *Event, nonce string) (*Event, error) {
	if nonce != "" {
		var ex Event
		var metaJSON string
		var encI int
		err := s.db.QueryRow(`SELECT id,seq,kind,actor,ts,body,meta,enc,epoch FROM events WHERE channel_id=? AND nonce=?`,
			e.Channel, nonce).Scan(&ex.ID, &ex.Seq, &ex.Kind, &ex.Actor, &ex.TS, &ex.Body, &metaJSON, &encI, &ex.Epoch)
		ex.Enc = encI == 1
		if err == nil {
			ex.Channel = e.Channel
			json.Unmarshal([]byte(metaJSON), &ex.Meta)
			return &ex, nil
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var next sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM events WHERE channel_id=?`, e.Channel).Scan(&next); err != nil {
		return nil, err
	}
	e.Seq = next.Int64 + 1
	e.ID = id("e")
	e.TS = now()
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	mj, _ := json.Marshal(e.Meta)
	encI := 0
	if e.Enc {
		encI = 1
	}
	if _, err := tx.Exec(`INSERT INTO events(id,channel_id,seq,kind,actor,ts,body,meta,nonce,enc,epoch) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Channel, e.Seq, e.Kind, e.Actor, e.TS, e.Body, string(mj), nonce, encI, e.Epoch); err != nil {
		return nil, err
	}
	return e, tx.Commit()
}

// Since returns events after cursor, oldest first, and applies no access rule
// of its own. The `sealed` history floor is the CALLER's job — handleSync raises
// the cursor to the member's joined_seq before asking. Stated plainly because
// the comment here used to claim this function did it, which is the kind of
// thing the next caller believes.
func (s *Store) Since(cid string, cursor int64, limit int) ([]*Event, error) {
	rows, err := s.r.Query(`SELECT id,seq,kind,actor,ts,body,meta,enc,epoch FROM events
	  WHERE channel_id=? AND seq>? ORDER BY seq LIMIT ?`, cid, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		e := &Event{Channel: cid}
		var mj string
		var encI int
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.Actor, &e.TS, &e.Body, &mj, &encI, &e.Epoch); err != nil {
			return nil, err
		}
		e.Enc = encI == 1
		json.Unmarshal([]byte(mj), &e.Meta)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastMessage is the most recent thing a PERSON said in a channel — not a join,
// not a mode change, which is never what "pin that" means. One row: /pin used
// to read the last 2000 events into memory and scan them backwards in Go.
func (s *Store) LastMessage(cid string) (*Event, error) {
	e := &Event{Channel: cid}
	var mj string
	var encI int
	err := s.r.QueryRow(`SELECT id,seq,kind,actor,ts,body,meta,enc,epoch FROM events
	  WHERE channel_id=? AND kind IN ('message','action') ORDER BY seq DESC LIMIT 1`, cid).
		Scan(&e.ID, &e.Seq, &e.Kind, &e.Actor, &e.TS, &e.Body, &mj, &encI, &e.Epoch)
	if err != nil {
		return nil, ErrNotFound
	}
	e.Enc = encI == 1
	json.Unmarshal([]byte(mj), &e.Meta)
	return e, nil
}

// HasEvent is whether that event id is in that channel. One row, no columns:
// it exists so a reply cannot claim to answer a message in a room the sender
// may not even be in.
func (s *Store) HasEvent(cid, id string) bool {
	var n int
	s.r.QueryRow(`SELECT 1 FROM events WHERE channel_id=? AND id=?`, cid, id).Scan(&n)
	return n == 1
}

func (s *Store) JoinedSeq(cid, uid string) int64 {
	var n int64
	s.r.QueryRow(`SELECT joined_seq FROM members WHERE channel_id=? AND user_id=?`, cid, uid).Scan(&n)
	return n
}

func (s *Store) SaveCursor(cid, uid string, seq int64) {
	s.db.Exec(`UPDATE members SET cursor=? WHERE channel_id=? AND user_id=? AND cursor<?`, seq, cid, uid, seq)
}

// SaveRead moves the read mark forward, never back. Grow-only is what makes it
// safe to accept from several devices at once without any ordering: two clients
// reporting different marks converge on the higher one, and a device that has
// been offline with a stale mark cannot un-read what another device read.
func (s *Store) SaveRead(cid, uid string, seq int64) {
	s.db.Exec(`UPDATE members SET read_seq=? WHERE channel_id=? AND user_id=? AND read_seq<?`, seq, cid, uid, seq)
}

// ReadSeq is how far this user has read in this channel.
func (s *Store) ReadSeq(cid, uid string) int64 {
	var n int64
	s.r.QueryRow(`SELECT read_seq FROM members WHERE channel_id=? AND user_id=?`, cid, uid).Scan(&n)
	return n
}

// ---- plugins ----

type Plugin struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Display string `json:"display"`
	Kind    string `json:"kind"`
}

func (s *Store) CreatePlugin(pid, display, kind string) (*Plugin, error) {
	p := &Plugin{ID: pid, Token: token(), Display: display, Kind: kind}
	_, err := s.db.Exec(`INSERT INTO plugins(id,token,display,kind,created_at) VALUES(?,?,?,?,?)`,
		p.ID, p.Token, display, kind, now())
	if err != nil {
		return nil, errors.New("plugin id already exists")
	}
	return p, nil
}

func (s *Store) PluginByToken(t string) (*Plugin, error) {
	p := &Plugin{}
	err := s.db.QueryRow(`SELECT id,token,display,kind FROM plugins WHERE token=?`, t).
		Scan(&p.ID, &p.Token, &p.Display, &p.Kind)
	if err != nil {
		return nil, ErrNotFound
	}
	return p, nil
}

func (s *Store) PluginJoin(pid, cid string) error {
	_, err := s.db.Exec(`INSERT INTO plugin_channels(plugin_id,channel_id) VALUES(?,?)
	  ON CONFLICT DO NOTHING`, pid, cid)
	return err
}

// PluginLeave removes a plugin from a channel for good: its subscribe list
// cannot bring it back (PluginKicked); an op's /invite can (PluginUnkick).
func (s *Store) PluginLeave(pid, cid string) error {
	// The marker goes in first and both land together, so a PluginSubscribe
	// running alongside either sees the kick or finds nothing to rejoin.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO plugin_kicks(plugin_id,channel_id) VALUES(?,?) ON CONFLICT DO NOTHING`, pid, cid); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plugin_channels WHERE plugin_id=? AND channel_id=?`, pid, cid); err != nil {
		return err
	}
	return tx.Commit()
}

// PluginSubscribe joins a plugin to a channel from its own subscribe list,
// unless an op removed it there: checked and written in one statement, so a
// /kick racing the register is never undone. Reports whether it joined.
func (s *Store) PluginSubscribe(pid, cid string) bool {
	r, err := s.db.Exec(`INSERT INTO plugin_channels(plugin_id,channel_id)
	  SELECT ?,? WHERE NOT EXISTS (SELECT 1 FROM plugin_kicks WHERE plugin_id=? AND channel_id=?)
	  ON CONFLICT DO NOTHING`, pid, cid, pid, cid)
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n > 0
}

// PluginInvite is an op's /invite: the kick marker goes and the membership
// comes, together or not at all.
func (s *Store) PluginInvite(pid, cid string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM plugin_kicks WHERE plugin_id=? AND channel_id=?`, pid, cid); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO plugin_channels(plugin_id,channel_id) VALUES(?,?) ON CONFLICT DO NOTHING`, pid, cid); err != nil {
		return err
	}
	return tx.Commit()
}

// SetPluginDisplay records the name a plugin registered with, so member lists
// show the same name an op invites it by.
func (s *Store) SetPluginDisplay(pid, display string) error {
	_, err := s.db.Exec(`UPDATE plugins SET display=? WHERE id=?`, display, pid)
	return err
}

func (s *Store) PluginInChannel(pid, cid string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM plugin_channels WHERE plugin_id=? AND channel_id=?`, pid, cid).Scan(&n)
	return n > 0
}

func (s *Store) PluginChannels(pid string) []string {
	rows, err := s.db.Query(`SELECT channel_id FROM plugin_channels WHERE plugin_id=?`, pid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// ---- uploads ----

type Upload struct {
	ID      string
	Channel string
	// Owner is the uploader's nick, for display (the admin console lists it).
	// OwnerID is who they ARE, and is the only thing an authorisation check may
	// look at — see the migration note in OpenStore.
	Owner    string
	OwnerID  string
	Name     string
	Size     int64
	Received int64
	Path     string
	Expires  string
	Complete bool
}

func (s *Store) CreateUpload(cid, ownerID, owner, name string, size int64, path string, ttl time.Duration) (*Upload, error) {
	u := &Upload{ID: id("f"), Channel: cid, Owner: owner, OwnerID: ownerID, Name: name, Size: size, Path: path,
		Expires: time.Now().UTC().Add(ttl).Format(time.RFC3339)}
	_, err := s.db.Exec(`INSERT INTO uploads(id,channel_id,owner,owner_id,name,size,path,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		u.ID, cid, owner, ownerID, name, size, path, u.Expires)
	return u, err
}

func (s *Store) Upload(uid string) (*Upload, error) {
	u := &Upload{}
	var complete int
	err := s.db.QueryRow(`SELECT id,channel_id,owner,owner_id,name,size,received,path,expires_at,complete FROM uploads WHERE id=?`, uid).
		Scan(&u.ID, &u.Channel, &u.Owner, &u.OwnerID, &u.Name, &u.Size, &u.Received, &u.Path, &u.Expires, &complete)
	if err != nil {
		return nil, ErrNotFound
	}
	u.Complete = complete == 1
	return u, nil
}

func (s *Store) UpdateUpload(uid string, received int64, complete bool) error {
	c := 0
	if complete {
		c = 1
	}
	_, err := s.db.Exec(`UPDATE uploads SET received=?, complete=? WHERE id=?`, received, c, uid)
	return err
}

// ExpiredUploads returns uploads past their expiry, for the sweeper.
func (s *Store) ExpiredUploads() []*Upload {
	rows, err := s.db.Query(`SELECT id,path FROM uploads WHERE expires_at < ?`, now())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Upload
	for rows.Next() {
		u := &Upload{}
		if rows.Scan(&u.ID, &u.Path) == nil {
			out = append(out, u)
		}
	}
	return out
}

func (s *Store) DeleteUpload(uid string) { s.db.Exec(`DELETE FROM uploads WHERE id=?`, uid) }

func (s *Store) CountUsers() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// ---- retro pack: per-user lists ----
//
// Aliases, highlight words and notify entries are the same shape — a short,
// per-user list that has to follow you between devices — so one table and one
// set of queries serves all three. The schema lives in retro.go.

// retroListMax caps every list. Fifty is past the point where a human still
// remembers what is in one, and well short of anything that costs the server.
const retroListMax = 50

func (s *Store) listPut(uid, kind, name, val string) error {
	s.retroSchema()
	// counted excluding this name, so overwriting an entry never hits the cap
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM user_lists WHERE user_id=? AND kind=? AND name<>?`,
		uid, kind, name).Scan(&n)
	if n >= retroListMax {
		return fmt.Errorf("you already have %d %s entries — remove one first", retroListMax, kind)
	}
	_, err := s.db.Exec(`INSERT INTO user_lists(user_id,kind,name,val) VALUES(?,?,?,?)
	  ON CONFLICT(user_id,kind,name) DO UPDATE SET val=excluded.val`, uid, kind, name, val)
	return err
}

func (s *Store) listDrop(uid, kind, name string) error {
	s.retroSchema()
	res, err := s.db.Exec(`DELETE FROM user_lists WHERE user_id=? AND kind=? AND name=?`, uid, kind, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such %s: %s", kind, name)
	}
	return nil
}

func (s *Store) listAll(uid, kind string) map[string]string {
	s.retroSchema()
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT name,val FROM user_lists WHERE user_id=? AND kind=?`, uid, kind)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n, v string
		if rows.Scan(&n, &v) == nil {
			out[n] = v
		}
	}
	return out
}

func (s *Store) SetAlias(uid, name, expansion string) error {
	return s.listPut(uid, "alias", name, expansion)
}

func (s *Store) RemoveAlias(uid, name string) error { return s.listDrop(uid, "alias", name) }

// AliasesFor is what the state frame ships to clients: the server stores the
// alias, the client expands it. Keyed by name without the leading slash.
func (s *Store) AliasesFor(uid string) map[string]string { return s.listAll(uid, "alias") }

func (s *Store) AddHighlight(uid, word string) error { return s.listPut(uid, "highlight", word, "") }

func (s *Store) RemoveHighlight(uid, word string) error { return s.listDrop(uid, "highlight", word) }

// HighlightsFor returns the words folded to lower case, sorted so the list a
// client renders does not reshuffle between syncs.
func (s *Store) HighlightsFor(uid string) []string {
	return sortedKeys(s.listAll(uid, "highlight"))
}

func (s *Store) AddNotify(uid, targetID string) error { return s.listPut(uid, "notify", targetID, "") }

func (s *Store) RemoveNotify(uid, targetID string) error { return s.listDrop(uid, "notify", targetID) }

// NotifyList resolves the watched user ids. A deleted account simply drops out
// of the list rather than showing up as a dangling id.
func (s *Store) NotifyList(uid string) []*User {
	var out []*User
	for _, target := range sortedKeys(s.listAll(uid, "notify")) {
		if u, err := s.UserByID(target); err == nil {
			out = append(out, u)
		}
	}
	return out
}

func (s *Store) watchersOf(targetID string) []string {
	s.retroSchema()
	rows, err := s.db.Query(`SELECT user_id FROM user_lists WHERE kind='notify' AND name=?`, targetID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			out = append(out, u)
		}
	}
	return out
}

// WatcherCount powers the "watched by N" line in /whois. A notify entry is
// opt-in and visible to the person being watched, so this number is theirs.
func (s *Store) WatcherCount(targetID string) int { return len(s.watchersOf(targetID)) }

// CtcpOK is the per-user privacy toggle for /ctcp. It fails open on a missing
// row: a lookup failure should not silently make someone unreachable.
func (s *Store) CtcpOK(uid string) bool {
	s.retroSchema()
	var v int
	if s.db.QueryRow(`SELECT ctcp_ok FROM users WHERE id=?`, uid).Scan(&v) != nil {
		return true
	}
	return v == 1
}

func (s *Store) SetCtcpOK(uid string, ok bool) error {
	s.retroSchema()
	v := 0
	if ok {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE users SET ctcp_ok=? WHERE id=?`, v, uid)
	return err
}

// ---- admin audit log (F14) ----

type AuditEntry struct {
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
	TS     string `json:"ts"`
}

func (s *Store) AppendAudit(actor, action, target, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (id,actor,action,target,detail,created_at) VALUES (?,?,?,?,?,?)`,
		id("aud"), actor, action, target, detail, now())
	return err
}

func (s *Store) AuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT actor,action,target,detail,created_at FROM audit_log ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Actor, &e.Action, &e.Target, &e.Detail, &e.TS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- server settings ----

// UnreadFor is what the switcher's bracket counts: how far every channel this
// account is in has moved past its own read mark, or past the moment they
// joined if that is later: history from before you arrived is not unread.
//
// Unread, never mentions. In a +e channel the body is ciphertext to the server,
// so it cannot tell whether your nick is in it — a count is the most any server
// can honestly report about a room it cannot read.
func (s *Store) UnreadFor(uid string) int64 {
	var n sql.NullInt64
	// MAX(seq) for a channel is one lookup on events_chan_seq, and a person is
	// in few channels, so this stays cheap however long the log gets.
	s.r.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN behind > 0 THEN behind ELSE 0 END), 0) FROM (
			SELECT (SELECT COALESCE(MAX(seq), 0) FROM events WHERE channel_id = m.channel_id)
			       - CASE WHEN m.joined_seq > m.read_seq THEN m.joined_seq ELSE m.read_seq END AS behind
			FROM members m WHERE m.user_id = ?
		) AS b`, uid).Scan(&n)
	return n.Int64
}

// Setting reads one setting, "" when it has never been set.
func (s *Store) Setting(key string) string {
	var v string
	s.r.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
