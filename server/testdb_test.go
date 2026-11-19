package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDSN is where a test's store lives: a SQLite file in its temp dir, or,
// with GLIPH_TEST_PG set to an admin URL, a fresh PostgreSQL database dropped
// afterwards. The whole suite runs against both:
//
//	GLIPH_TEST_PG=postgres://localhost/postgres go test ./...
func testDSN(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("GLIPH_TEST_PG")
	if admin == "" {
		return filepath.Join(t.TempDir(), "t.db")
	}
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	name := "gliph_t_" + strings.ToLower(id("d")[2:])
	if _, err := db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
		db.Close()
	})
	i := strings.LastIndex(admin, "/")
	return admin[:i+1] + name
}
