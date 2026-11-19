package main

import (
	"database/sql"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The seam between this package's SQL and the database under it.
//
// Every query in server/ is written once, in the dialect both databases share:
// `?` placeholders, ON CONFLICT upserts, no rowid, no SQLite-only functions.
// What still differs is spelled out in the four rewrites below and nowhere else.
// A self-host runs on one SQLite file; gliph.sh runs on Pushleft's managed
// PostgreSQL, because a volume makes every deploy stop the old instance first.
//
// Not an interface with two implementations: the same *sql.DB, with the query
// text adjusted on its way to the driver.
type DB struct {
	*sql.DB
	pg bool
}

type Tx struct {
	*sql.Tx
	pg bool
}

func isPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func (d *DB) Exec(q string, a ...any) (sql.Result, error) { return d.DB.Exec(rewrite(d.pg, q), a...) }
func (d *DB) Query(q string, a ...any) (*sql.Rows, error) { return d.DB.Query(rewrite(d.pg, q), a...) }
func (d *DB) QueryRow(q string, a ...any) *sql.Row        { return d.DB.QueryRow(rewrite(d.pg, q), a...) }
func (d *DB) Begin() (*Tx, error) {
	tx, err := d.DB.Begin()
	return &Tx{tx, d.pg}, err
}

func (t *Tx) Exec(q string, a ...any) (sql.Result, error) { return t.Tx.Exec(rewrite(t.pg, q), a...) }
func (t *Tx) Query(q string, a ...any) (*sql.Rows, error) { return t.Tx.Query(rewrite(t.pg, q), a...) }
func (t *Tx) QueryRow(q string, a ...any) *sql.Row        { return t.Tx.QueryRow(rewrite(t.pg, q), a...) }

// rewrite turns the shared dialect into PostgreSQL's. SQLite gets it verbatim.
func rewrite(pg bool, q string) string {
	if !pg {
		return q
	}
	return numberPlaceholders(q)
}

// ddl adapts schema statements. SQLite's INTEGER is 64-bit; PostgreSQL's is
// 32, so a seq or a byte count would overflow there. Booleans stay 0/1
// integers on both: every scan site already reads them that way.
func ddl(pg bool, q string) string {
	if !pg {
		return q
	}
	var keep []string
	for _, line := range strings.Split(q, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PRAGMA") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.ReplaceAll(strings.Join(keep, "\n"), " INTEGER ", " BIGINT ")
}

// dupColumn is the "this ALTER already ran" error, in either dialect.
func dupColumn(err error) bool {
	s := err.Error()
	return strings.Contains(s, "duplicate column") || strings.Contains(s, "already exists")
}

// numberPlaceholders turns `?` into `$1, $2, …`, skipping string literals:
// a `?` inside quotes is data. ” is SQL's own escape and keeps us inside.
func numberPlaceholders(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case c == '?' && !quoted:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// dbBytes is the database's size on disk, for /metrics and the admin console.
func (s *Store) dbBytes() int64 {
	if s.db.pg {
		return s.metricCount(`SELECT pg_database_size(current_database())`)
	}
	return s.metricCount(`SELECT page_count*page_size FROM pragma_page_count(), pragma_page_size()`)
}
