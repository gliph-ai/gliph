package main

// Moving a team between shards (docs/control.md). Every team is a schema of
// the one PostgreSQL and a prefix of the one bucket, so a move copies nothing:
// the source shard lets go of the team and the target picks it up from the
// registry. The control plane runs both halves as one operation
// (control/move.go):
//
//	POST /shard/tenants/{slug}/release   on the source: drain and let go
//	POST /shard/tenants/{slug}/adopt     on the target: claim and serve
//
// Between the two, the team's addresses answer 503 on the source, so a client
// retries instead of treating the team as gone. A release followed by a failed
// adopt is undone by adopting on the source again.

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
)

var errNotReleased = errors.New("another shard still serves that team")

// release drains a team and hands it back to the registry: its sockets
// close, new requests get 503, its connections close once the queries in
// flight finish (sql.DB.Close waits for them), and its registry row names no
// shard. Releasing a team this shard does not serve is a no-op, so a retried
// move can run it again.
func (sh *Shard) release(slug string) error {
	sh.provMu.Lock()
	defer sh.provMu.Unlock()
	if sh.solo != nil {
		return errors.New("this server runs a single team and has none to move")
	}
	sh.mu.Lock()
	var t *Tenant
	for i, x := range sh.list {
		if x.Slug == slug {
			t = x
			sh.list = append(sh.list[:i:i], sh.list[i+1:]...)
			break
		}
	}
	if t == nil {
		sh.mu.Unlock()
		return nil
	}
	if sh.moving == nil {
		sh.moving = map[string]bool{}
	}
	for _, h := range t.Hosts {
		delete(sh.byHost, hostKey(h))
		sh.moving[hostKey(h)] = true
	}
	sh.mu.Unlock()

	t.Srv.dropAll()
	reg, err := sh.registry()
	if err == nil {
		_, err = reg.Exec(`UPDATE gliph_tenants SET shard='' WHERE slug=$1 AND shard=$2`, slug, sh.opts.Name)
	}
	t.St.Close()
	if err != nil {
		return err
	}
	log.Printf("shard: released %s", slug)
	return nil
}

// adopt claims a released team in the registry and serves it. A team already
// served here is a success; one another shard still holds is refused.
func (sh *Shard) adopt(slug string) (*Tenant, error) {
	sh.provMu.Lock()
	defer sh.provMu.Unlock()
	if sh.solo != nil {
		return nil, errors.New("this server runs a single team and cannot host more")
	}
	for _, t := range sh.Tenants() {
		if t.Slug == slug {
			return t, nil
		}
	}
	reg, err := sh.registry()
	if err != nil {
		return nil, err
	}
	var tf tenantFile
	var hosts string
	err = reg.QueryRow(`UPDATE gliph_tenants SET shard=$2 WHERE slug=$1 AND shard IN ('', $2)
	  RETURNING name, hosts, suspended`, slug, sh.opts.Name).Scan(&tf.Name, &hosts, &tf.Suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotReleased
	}
	if err != nil {
		return nil, err
	}
	t, err := sh.Open(slug, tf.Name, strings.Split(hosts, ","))
	if err != nil {
		// Unclaimed again, so the source can take it back.
		reg.Exec(`UPDATE gliph_tenants SET shard='' WHERE slug=$1 AND shard=$2`, slug, sh.opts.Name)
		return nil, err
	}
	t.suspended.Store(tf.Suspended)
	sh.mu.Lock()
	for _, h := range t.Hosts {
		delete(sh.moving, hostKey(h))
	}
	sh.mu.Unlock()
	log.Printf("shard: adopted %s at %s", slug, hosts)
	return t, nil
}

func (sh *Shard) isMoving(host string) bool {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.moving[hostKey(host)]
}

// moveRoutes answers the two halves; shardRoutes has already checked the secret.
func (sh *Shard) moveRoutes(w http.ResponseWriter, slug, op string) {
	switch op {
	case "release":
		if err := sh.release(slug); err != nil {
			fail(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"slug": slug, "released": true})
	case "adopt":
		t, err := sh.adopt(slug)
		switch {
		case errors.Is(err, errNotReleased):
			fail(w, 409, err.Error())
		case err != nil:
			fail(w, 500, err.Error())
		default:
			writeJSON(w, 200, map[string]any{"slug": t.Slug, "hosts": t.Hosts})
		}
	}
}
