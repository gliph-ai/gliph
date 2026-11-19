package main

import (
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A shard: one process, many teams.
//
// A Gliph server IS a team — its own tables, its own accounts, its own
// #general — so a tenant here is exactly what this whole process used to be,
// and a shard is a map of them keyed by the Host header. Hosted only: each
// team is a PostgreSQL schema and a bucket prefix (see home).
//
// That map lookup is the ONLY place tenancy exists. Everything below it —
// every query in store.go, every frame in server.go — is the same
// single-tenant code it always was, holding a *Server that belongs to one team.
// Compare with a tenant_id column, where each of ~120 queries is its own chance
// to leak one team's messages into another's: one place to get right beats a
// hundred.
//
// A self-hosted server never shards: it is the solo team, one SQLite file,
// every Host. Same code, map lookup skipped.

// Tenant is one team on this shard.
type Tenant struct {
	Slug string
	// Home names where this team's data is: its directory on disk, or its
	// schema and bucket prefix. Moving a team is moving exactly that.
	Home  string
	Hosts []string
	Name  string

	St  *Store
	Srv *Server
	h   http.Handler
	// suspended is billing, decided by the control plane and pushed here
	// (POST /shard/tenants/{slug}/status): every request is refused, so a
	// session minted before the suspension is worth nothing during it.
	suspended atomic.Bool
}

// tenantFile is the shard's record of a team, a row of gliph_tenants, so a
// restart knows who it serves without asking the control plane.
type tenantFile struct {
	Slug, Name string
	Hosts      []string
	Suspended  bool
}

// ShardOpts is what every tenant on this shard shares. None of it is per-team:
// one APNs connection, one TURN relay, one copy of the web bundle.
type ShardOpts struct {
	WebDir      string
	DownloadDir string
	Version     string
	Started     time.Time
	Senders     byPlatform
	Mail        Mailer
	ICE         ICEConfig
	// Secret authorises the provisioning API. Empty means this shard accepts no
	// provisioning at all, which is what a self-hosted server wants.
	Secret string
	// Where teams live. Sharding is hosted-only: every team is a schema of PG
	// (a postgres:// URL) and its files a prefix of Bucket. The solo team, a
	// self-host or gliph.sh's own, is PG's public schema and the bucket's
	// root, or without PG a SQLite file and files in DataDir.
	DataDir string
	PG      string
	Bucket  *s3Files
	// Name is this shard's stable name (-shard-name). Every shard records
	// its teams in the one registry, so each loads only the rows naming it.
	Name string
	// ControlKey is the control plane's ed25519 public key. Empty — the
	// default, and what every self-hosted server runs with — means this server
	// accepts no single sign-on at all and keeps passwords and invites.
	ControlKey ed25519.PublicKey
}

type Shard struct {
	// provMu serialises provisioning in this process; the registry's
	// constraints settle a race between processes (claimTenant).
	provMu sync.Mutex
	// statusMu keeps a status's registry write and its in-memory flag in step.
	statusMu sync.Mutex

	reg     *sql.DB
	regOnce sync.Once
	regErr  error

	mu     sync.RWMutex
	byHost map[string]*Tenant
	list   []*Tenant
	// solo is set when this process serves ONE team whatever the Host arrives:
	// a self-hosted server, and every deployment that existed before shards.
	solo *Tenant
	// moving holds the addresses of teams released to another shard
	// (shardmove.go): they answer 503 here, never 404, while the move runs.
	moving map[string]bool
	opts   ShardOpts
}

func NewShard(opts ShardOpts) *Shard {
	return &Shard{byHost: map[string]*Tenant{}, opts: opts}
}

// hostKey is how a request finds its team. Ports and capitals are not part of
// the identity: `Acme.Gliph.Chat:443` and `acme.gliph.chat` are one team, and
// treating them as two is a tenant that intermittently 404s behind a proxy.
func hostKey(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndex(h, ":"); i > strings.LastIndex(h, "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// Open brings a tenant up: its database, its server, its routes. This is the
// body the single-tenant main() used to be. slug "" is the solo team.
func (sh *Shard) Open(slug, name string, hosts []string) (*Tenant, error) {
	dsn, fs, home, err := sh.home(slug)
	if err != nil {
		return nil, err
	}
	st, err := OpenStore(dsn)
	if err != nil {
		return nil, err
	}
	st.inviteOnly = slug != ""
	if name == "" {
		name = slug
	}
	srv := NewServer(st, name)
	srv.Version = sh.opts.Version
	srv.Started = sh.opts.Started
	srv.Push = newPushHub(st, sh.opts.Senders)
	srv.Mail = sh.opts.Mail
	srv.DownloadDir = sh.opts.DownloadDir
	srv.ICE = sh.opts.ICE
	srv.Files = fs

	// #general always exists so a fresh team has somewhere to land.
	if _, err := st.ChannelByName("general"); err != nil {
		if _, err := st.CreateChannel("general", "welcome to gliph. type / for every command", false); err != nil {
			log.Printf("%s: could not create #general: %v", slug, err)
		}
	}

	t := &Tenant{Slug: slug, Home: home, Hosts: hosts, Name: name, St: st, Srv: srv}
	t.h = srv.Routes(sh.opts.WebDir, filepath.Join(home, "uploads"))

	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, h := range hosts {
		k := hostKey(h)
		if k == "" {
			continue
		}
		if other, taken := sh.byHost[k]; taken && other.Slug != slug {
			st.Close()
			return nil, fmt.Errorf("%s is already served by %s", k, other.Slug)
		}
		sh.byHost[k] = t
	}
	sh.list = append(sh.list, t)
	return t, nil
}

// home resolves a team to its database, its file store and a name for both.
//
// A team is a schema, t_<slug>, selected by search_path on every
// connection the team's Store opens. That keeps the property the directories
// had: no query in this package names a tenant, so none can name the wrong one.
func (sh *Shard) home(slug string) (dsn string, fs files, home string, err error) {
	o := sh.opts
	switch {
	case o.Bucket != nil:
		fs = o.Bucket.under(tenantPrefix(slug))
	case slug == "":
		fs = diskFiles(o.DataDir)
	default:
		// A team without the bucket only happens in a test: on Pushleft this
		// disk does not outlive a deploy, and main refuses -shard without one.
		fs = diskFiles(filepath.Join(o.DataDir, "tenants", slug))
	}
	if slug == "" {
		if o.PG != "" {
			return o.PG, fs, "public", nil
		}
		if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
			return "", nil, "", err
		}
		return filepath.Join(o.DataDir, "gliph.db"), fs, o.DataDir, nil
	}
	if o.PG == "" {
		return "", nil, "", errors.New("teams on a shard need PostgreSQL")
	}
	schema := tenantSchema(slug)
	reg, err := sh.registry()
	if err != nil {
		return "", nil, "", err
	}
	if _, err := reg.Exec(`CREATE SCHEMA IF NOT EXISTS ` + schema); err != nil {
		return "", nil, "", err
	}
	return withParam(o.PG, "search_path", schema), fs, schema, nil
}

// tenantSchema is safe to splice into SQL: validSlug allows only a-z, 0-9 and
// "-", and "_" never appears in a slug, so two slugs never share a schema.
func tenantSchema(slug string) string { return "t_" + strings.ReplaceAll(slug, "-", "_") }

func tenantPrefix(slug string) string {
	if slug == "" {
		return ""
	}
	return "tenants/" + slug + "/"
}

func withParam(dsn, k, v string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set(k, v)
	u.RawQuery = q.Encode()
	return u.String()
}

// registry is the shard's own connection to PostgreSQL's public schema, where
// gliph_tenants lists the teams. Opened on first use.
func (sh *Shard) registry() (*sql.DB, error) {
	if sh.opts.PG == "" {
		return nil, errors.New("teams on a shard need PostgreSQL")
	}
	sh.regOnce.Do(func() {
		sh.reg, sh.regErr = sql.Open("pgx", sh.opts.PG)
		if sh.regErr == nil {
			_, sh.regErr = sh.reg.Exec(`CREATE TABLE IF NOT EXISTS gliph_tenants (
			  slug      TEXT PRIMARY KEY,
			  name      TEXT NOT NULL DEFAULT '',
			  hosts     TEXT NOT NULL DEFAULT '',
			  suspended BOOLEAN NOT NULL DEFAULT false,
			  invite    TEXT NOT NULL DEFAULT '',
			  shard     TEXT NOT NULL DEFAULT '')`)
		}
		for _, q := range []string{
			`ALTER TABLE gliph_tenants ADD COLUMN IF NOT EXISTS suspended BOOLEAN NOT NULL DEFAULT false`,
			`ALTER TABLE gliph_tenants ADD COLUMN IF NOT EXISTS invite TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE gliph_tenants ADD COLUMN IF NOT EXISTS shard TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE gliph_tenants ADD COLUMN IF NOT EXISTS status_version BIGINT NOT NULL DEFAULT 0`,
			// One row per address, so two teams can never claim one, across
			// every shard sharing this registry, by a constraint rather than a
			// check that two requests can both pass.
			`CREATE TABLE IF NOT EXISTS gliph_hosts (
			  host TEXT PRIMARY KEY,
			  slug TEXT NOT NULL REFERENCES gliph_tenants(slug) ON DELETE CASCADE)`,
		} {
			if sh.regErr == nil {
				_, sh.regErr = sh.reg.Exec(q)
			}
		}
	})
	return sh.reg, sh.regErr
}

var (
	errTeamExists = errors.New("a team with that slug already exists")
	errHostTaken  = errors.New("that address is already served by another team")
)

// claimTenant records a team and its addresses in one transaction, before the
// team opens, so a crash between the two leaves a record the next boot picks
// up rather than data nothing knows about. The primary keys are the claim:
// a second team with the slug, or an address already taken, fails the insert
// whatever another request or another shard is doing at the same moment.
func (sh *Shard) claimTenant(tf tenantFile) error {
	reg, err := sh.registry()
	if err != nil {
		return err
	}
	tx, err := reg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO gliph_tenants(slug,name,hosts,shard) VALUES($1,$2,$3,$4)
	  ON CONFLICT(slug) DO NOTHING`, tf.Slug, tf.Name, strings.Join(tf.Hosts, ","), sh.opts.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errTeamExists
	}
	for _, h := range tf.Hosts {
		if _, err := tx.Exec(`INSERT INTO gliph_hosts(host,slug) VALUES($1,$2)`, hostKey(h), tf.Slug); err != nil {
			return errHostTaken
		}
	}
	return tx.Commit()
}

func (sh *Shard) forgetTenant(slug string) {
	if reg, err := sh.registry(); err == nil {
		reg.Exec(`DELETE FROM gliph_tenants WHERE slug=$1`, slug)
	}
}

func (sh *Shard) savedTenants() ([]tenantFile, error) {
	reg, err := sh.registry()
	if err != nil {
		return nil, err
	}
	rows, err := reg.Query(`SELECT slug,name,hosts,suspended FROM gliph_tenants WHERE shard=$1 ORDER BY slug`, sh.opts.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenantFile
	for rows.Next() {
		var tf tenantFile
		var hosts string
		if err := rows.Scan(&tf.Slug, &tf.Name, &hosts, &tf.Suspended); err != nil {
			return nil, err
		}
		tf.Hosts = strings.Split(hosts, ",")
		out = append(out, tf)
	}
	return out, rows.Err()
}

// Solo makes this process serve one team whatever Host arrives.
func (sh *Shard) Solo(t *Tenant) {
	sh.mu.Lock()
	sh.solo = t
	sh.mu.Unlock()
}

// Load brings up every team this shard has a record of, so a restart needs
// nothing from the control plane to serve the teams it was serving a second ago.
func (sh *Shard) Load() error {
	tfs, err := sh.savedTenants()
	if err != nil {
		return err
	}
	for _, tf := range tfs {
		t, err := sh.Open(tf.Slug, tf.Name, tf.Hosts)
		if err != nil {
			// Fail the boot rather than serve without a team that is on
			// record: a shard that comes up healthy with one team missing
			// takes the deploy, and that team is down until someone notices.
			return fmt.Errorf("shard: could not open %s: %w", tf.Slug, err)
		}
		t.suspended.Store(tf.Suspended)
		log.Printf("shard: serving %s at %s", tf.Slug, strings.Join(tf.Hosts, ", "))
	}
	return nil
}

// Tenants is a snapshot of who this shard serves.
func (sh *Shard) Tenants() []*Tenant {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return append([]*Tenant(nil), sh.list...)
}

func (sh *Shard) tenantFor(host string) *Tenant {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.solo != nil {
		return sh.solo
	}
	return sh.byHost[hostKey(host)]
}

func (sh *Shard) Close() {
	for _, t := range sh.Tenants() {
		t.St.Close()
	}
	if sh.reg != nil {
		sh.reg.Close()
	}
}

// Housekeeping runs every tenant's sweeps on one timer rather than one timer
// per team: at a few hundred teams that is the difference between a tick and a
// thundering herd.
func (sh *Shard) Housekeeping() {
	for range time.Tick(10 * time.Minute) {
		for _, t := range sh.Tenants() {
			t.Srv.sweepUploads()
			t.Srv.Limits.Prune()
			t.Srv.Push.prune()
			// A token nothing has touched in three months belongs to an app
			// that is gone.
			t.St.PrunePushDevices(90 * 24 * time.Hour)
			// UserByToken already refuses an expired session; this is so the
			// table does not keep a row for every tab ever opened.
			t.St.PruneSessions()
			t.St.PruneDeviceLinks()
			t.St.PruneSpentAssertions(time.Now())
			// Mails anyone who has been away long enough to have missed
			// something, at most once per absence. No-op without -ses-key-id.
			t.Srv.sendDigests()
		}
	}
}

// ---- provisioning ----

// ServeHTTP resolves the team ONCE, at the edge, and hands the request to code
// that has never heard of tenancy.
func (sh *Shard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/shard/") {
		sh.shardRoutes(w, r)
		return
	}
	t := sh.tenantFor(r.Host)
	if t == nil && sh.isMoving(r.Host) {
		w.Header().Set("Retry-After", "5")
		fail(w, 503, "this team is moving; try again in a moment")
		return
	}
	if t == nil {
		// An unknown Host is a 404 and never a tenant created on the spot.
		// Wildcard DNS points every name on the domain here, so lazily creating
		// one would let anybody at all spawn databases on this disk.
		fail(w, 404, "no server at this address")
		return
	}
	if t.suspended.Load() {
		fail(w, 402, "this team is suspended")
		return
	}
	// Handled here rather than inside the tenant's routes because the signing
	// key belongs to the shard, not to any one team — and because the audience
	// check needs the slug the Host resolved to, which only this layer knows.
	if r.URL.Path == "/api/sso" && r.Method == http.MethodPost {
		sh.sso(w, r, t)
		return
	}
	t.h.ServeHTTP(w, r)
}

// sso trades a control-plane assertion for an ordinary session on this team.
func (sh *Shard) sso(w http.ResponseWriter, r *http.Request, t *Tenant) {
	if len(sh.opts.ControlKey) == 0 {
		fail(w, 404, "this server does not accept single sign-on")
		return
	}
	var d struct {
		Assertion string `json:"assertion"`
	}
	json.NewDecoder(r.Body).Decode(&d)
	c, err := verifyAssertion(sh.opts.ControlKey, t.Slug, d.Assertion, time.Now())
	if err != nil {
		fail(w, 403, err.Error())
		return
	}
	if err := t.St.SpendAssertion(c.Jti, c.Exp); err != nil {
		fail(w, 403, err.Error())
		return
	}
	tok, u, err := t.St.SSOSession(c.Email, c.Nick, c.Role == "owner")
	if err != nil {
		fail(w, 403, err.Error())
		return
	}
	// Everyone lands in #general, the same way a registration does.
	if ch, err := t.St.ChannelByName("general"); err == nil {
		if _, already := t.St.MemberRole(ch.ID, u.ID); !already {
			t.St.Join(ch.ID, u.ID, "member")
			t.Srv.Post(ch, &Event{Kind: "join", Actor: u.Nick, Body: "joined the server",
				Meta: map[string]any{"new": true}}, "")
			t.Srv.PushChannelState(ch.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"token": tok, "user": u})
}

func (sh *Shard) shardRoutes(w http.ResponseWriter, r *http.Request) {
	// One answer for "no secret configured", "no token" and "wrong token":
	// telling them apart turns this into an oracle for whether a shard accepts
	// provisioning and what its secret looks like.
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if sh.opts.Secret == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sh.opts.Secret)) != 1 {
		fail(w, 403, "not authorised")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/shard/tenants":
		out := []map[string]any{}
		for _, t := range sh.Tenants() {
			out = append(out, map[string]any{
				"slug": t.Slug, "name": t.Name, "hosts": t.Hosts,
				"users": t.St.CountUsers(),
			})
		}
		writeJSON(w, 200, map[string]any{"tenants": out, "solo": sh.solo != nil})

	case r.Method == http.MethodPost && r.URL.Path == "/shard/tenants":
		sh.provision(w, r)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/shard/tenants/") &&
		(strings.HasSuffix(r.URL.Path, "/release") || strings.HasSuffix(r.URL.Path, "/adopt")):
		slug, op, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/shard/tenants/"), "/")
		sh.moveRoutes(w, slug, op)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/shard/tenants/") &&
		strings.HasSuffix(r.URL.Path, "/status"):
		sh.setStatus(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/shard/tenants/"), "/status"))

	default:
		fail(w, 404, "no such shard route")
	}
}

// provision creates a team on this shard and hands back the invite its first
// account will use. It is deliberately the ONLY way a tenant comes into
// existence at runtime.
func (sh *Shard) provision(w http.ResponseWriter, r *http.Request) {
	sh.provMu.Lock()
	defer sh.provMu.Unlock()
	if sh.solo != nil {
		fail(w, 409, "this server runs a single team and cannot host more")
		return
	}
	var d struct {
		Slug  string   `json:"slug"`
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
	}
	json.NewDecoder(r.Body).Decode(&d)
	if !validSlug(d.Slug) {
		fail(w, 400, "slug must be 1-40 characters of a-z, 0-9 and -")
		return
	}
	if len(d.Hosts) == 0 {
		fail(w, 400, "a team needs at least one host")
		return
	}

	sh.mu.RLock()
	for _, t := range sh.list {
		if t.Slug == d.Slug {
			sh.mu.RUnlock()
			// A retry asks for the same addresses. Different ones are a
			// different request, and answering it with this team's invite
			// would hand out a team that does not answer where it was asked.
			if !sameHosts(t.Hosts, d.Hosts) {
				fail(w, 409, "that slug is taken with other addresses")
				return
			}
			// Idempotent: the control plane retries, and a retry that made a
			// second team would be worse than one that failed. The retry gets
			// the same owner invite, in case the first answer was lost.
			inv := sh.savedInvite(t.Slug)
			if inv == "" && t.St.CountUsers() == 0 {
				// The first attempt stopped after the team opened and before
				// its invite was saved. Nobody can be in it without one, so
				// minting it now gives nothing away.
				var err error
				if inv, err = sh.ownerInvite(t); err != nil {
					fail(w, 500, err.Error())
					return
				}
			}
			writeJSON(w, 200, map[string]any{"slug": t.Slug, "hosts": t.Hosts, "existing": true, "invite": inv})
			return
		}
	}
	// Before anything is recorded: a team refused for a taken host must leave
	// no record behind for the next boot to trip over.
	for _, h := range d.Hosts {
		if other, taken := sh.byHost[hostKey(h)]; taken {
			sh.mu.RUnlock()
			fail(w, 409, fmt.Sprintf("%s is already served by %s", hostKey(h), other.Slug))
			return
		}
	}
	sh.mu.RUnlock()

	switch err := sh.claimTenant(tenantFile{Slug: d.Slug, Name: d.Name, Hosts: d.Hosts}); {
	case errors.Is(err, errTeamExists), errors.Is(err, errHostTaken):
		// Taken here, or by another shard sharing the registry.
		fail(w, 409, err.Error())
		return
	case err != nil:
		fail(w, 500, err.Error())
		return
	}
	t, err := sh.Open(d.Slug, d.Name, d.Hosts)
	if err != nil {
		sh.forgetTenant(d.Slug) // its hosts go with it (ON DELETE CASCADE)
		fail(w, 500, err.Error())
		return
	}
	// The first account on a fresh team is its admin (see Store.Register), so
	// the invite this returns is what makes whoever redeems it the owner.
	invite, err := sh.ownerInvite(t)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("shard: provisioned %s at %s", t.Slug, strings.Join(t.Hosts, ", "))
	writeJSON(w, 200, map[string]any{"slug": t.Slug, "hosts": t.Hosts, "invite": invite})
}

// setStatus is how the control plane's billing decision reaches the team. The
// record is written first, so a restart keeps it; then the live sockets go.
func (sh *Shard) setStatus(w http.ResponseWriter, r *http.Request, slug string) {
	var d struct {
		Suspended bool  `json:"suspended"`
		Version   int64 `json:"version"`
	}
	json.NewDecoder(r.Body).Decode(&d)
	sh.statusMu.Lock()
	defer sh.statusMu.Unlock()
	var t *Tenant
	for _, x := range sh.Tenants() {
		if x.Slug == slug {
			t = x
		}
	}
	if t == nil {
		fail(w, 404, "no such team on this shard")
		return
	}
	// Versioned by the control plane: a push older than the one already
	// applied is a late arrival, acknowledged and ignored, so two pushes that
	// cross on the way cannot leave the team on the stale status.
	reg, err := sh.registry()
	var res sql.Result
	if err == nil {
		res, err = reg.Exec(`UPDATE gliph_tenants SET suspended=$1, status_version=$3
		  WHERE slug=$2 AND status_version < $3`, d.Suspended, slug, d.Version)
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, 200, map[string]any{"slug": slug, "stale": true})
		return
	}
	t.suspended.Store(d.Suspended)
	if d.Suspended {
		t.Srv.dropAll()
	}
	writeJSON(w, 200, map[string]any{"slug": slug, "suspended": d.Suspended})
}

// ownerInvite mints the admin invite a hosted team's owner redeems, and keeps
// it in the registry so a retried provisioning returns the same one.
func (sh *Shard) ownerInvite(t *Tenant) (string, error) {
	invite, err := t.St.CreateInvite("admin")
	if err != nil {
		return "", err
	}
	reg, err := sh.registry()
	if err == nil {
		_, err = reg.Exec(`UPDATE gliph_tenants SET invite=$1 WHERE slug=$2`, invite, t.Slug)
	}
	return invite, err
}

// savedInvite is the owner invite a team was provisioned with.
func (sh *Shard) savedInvite(slug string) string {
	var inv string
	if reg, err := sh.registry(); err == nil {
		reg.QueryRow(`SELECT invite FROM gliph_tenants WHERE slug=$1`, slug).Scan(&inv)
	}
	return inv
}

func sameHosts(a, b []string) bool {
	norm := func(hs []string) string {
		var k []string
		for _, h := range hs {
			k = append(k, hostKey(h))
		}
		sort.Strings(k)
		return strings.Join(k, ",")
	}
	return norm(a) == norm(b)
}

// validSlug is a DNS label: a-z, 0-9 and "-", 1-40 long, never starting or
// ending with "-" (-.gliph.chat is not a hostname).
func validSlug(s string) bool {
	if s == "" || len(s) > 40 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
