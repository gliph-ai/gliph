package main

// Moving a team to another shard: drain, move, repoint, resume, as one call.
//
//	POST /api/servers/{slug}/move {"shard":"b"}     (admin secret)
//	control -move acme:b                             (the same, from a shell)
//
// Every team is a schema of the one PostgreSQL and a prefix of the one bucket,
// so nothing is copied (server/shardmove.go). The source shard releases the
// team (its sockets close, its address answers 503), the target adopts it from
// the shards' registry, and this directory then names the target. The team's
// address names the team, not the shard, so no client changes anything.
//
// A failed adopt is resumed on the source, so a move that cannot finish leaves
// the team where it was. Every step is idempotent: a move that died halfway is
// finished by running it again.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
)

func shardOp(sh *Shard, slug, op string) error {
	req, err := http.NewRequest("POST", sh.BaseURL+"/shard/tenants/"+slug+"/"+op, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+sh.Secret)
	res, err := shardHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("shard %s: %w", sh.Name, err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("shard %s refused %s (%d)", sh.Name, op, res.StatusCode)
	}
	return nil
}

func (s *Store) ShardByName(name string) (*Shard, error) {
	sh := &Shard{}
	err := s.db.QueryRow(`SELECT name, base_url, secret FROM shards WHERE name=$1`, name).
		Scan(&sh.Name, &sh.BaseURL, &sh.Secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sh, err
}

func (s *Store) MoveServer(slug, shard string) error {
	_, err := s.db.Exec(`UPDATE servers SET shard=$2 WHERE slug=$1`, slug, shard)
	return err
}

// Move runs the whole operation. The shard the directory names is the source.
func (a *API) Move(slug, to string) error {
	src, err := a.St.ShardFor(slug)
	if err != nil {
		return fmt.Errorf("no team %q: %w", slug, err)
	}
	dst, err := a.St.ShardByName(to)
	if err != nil {
		return fmt.Errorf("no shard %q: %w", to, err)
	}
	if src.Name != dst.Name {
		if err := shardOp(src, slug, "release"); err != nil {
			return err
		}
		if err := shardOp(dst, slug, "adopt"); err != nil {
			if back := shardOp(src, slug, "adopt"); back != nil {
				return fmt.Errorf("%v; and resuming on %s failed: %v (run the move again)", err, src.Name, back)
			}
			return fmt.Errorf("%v; %s still serves %s", err, src.Name, slug)
		}
	} else if err := shardOp(dst, slug, "adopt"); err != nil {
		return err // a retry after the directory was updated: make sure it is served
	}
	return a.St.MoveServer(slug, dst.Name)
}
