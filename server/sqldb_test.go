package main

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPlaceholdersAreNumberedForPostgres(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`SELECT * FROM users WHERE id=?`, `SELECT * FROM users WHERE id=$1`},
		{`UPDATE members SET read_seq=? WHERE channel_id=? AND user_id=?`,
			`UPDATE members SET read_seq=$1 WHERE channel_id=$2 AND user_id=$3`},
		// A ? inside a string literal is DATA. Rewriting one would corrupt the
		// query silently instead of failing.
		{`SELECT * FROM events WHERE body='what?' AND channel_id=?`,
			`SELECT * FROM events WHERE body='what?' AND channel_id=$1`},
		// '' is SQL's own escape for a quote and keeps us inside the literal.
		{`SELECT * FROM t WHERE a='it''s a ?' AND b=?`,
			`SELECT * FROM t WHERE a='it''s a ?' AND b=$1`},
	}
	for _, c := range cases {
		if got := rewrite(true, c.in); got != c.want {
			t.Errorf("rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		// Everything in this package is written in the shared dialect, so on
		// SQLite the rewrite must change nothing at all.
		if got := rewrite(false, c.in); got != c.in {
			t.Errorf("sqlite rewrote %q into %q", c.in, got)
		}
	}
}

func TestTheDialectComesFromTheDSN(t *testing.T) {
	for dsn, want := range map[string]bool{
		"./data/gliph.db":                   false,
		"/var/lib/gliph/gliph.db":           false,
		"postgres://user@host:5432/gliph":   true,
		"postgresql://user@host:5432/gliph": true,
	} {
		if got := isPostgres(dsn); got != want {
			t.Errorf("isPostgres(%q) = %v, want %v", dsn, got, want)
		}
	}
}

// SQLite-only constructs. A query using one runs on a self-host and fails on
// gliph.sh, so none may appear in this package's SQL. The suite also runs
// against PostgreSQL (GLIPH_TEST_PG); this catches the query no test reaches.
var sqliteOnly = []struct {
	name string
	re   *regexp.Regexp
}{
	{"rowid", regexp.MustCompile(`(?i)\browid\b`)},
	{"strftime", regexp.MustCompile(`(?i)\bstrftime\s*\(`)},
	{"julianday", regexp.MustCompile(`(?i)\bjulianday\s*\(`)},
	{"AUTOINCREMENT", regexp.MustCompile(`(?i)\bautoincrement\b`)},
	{"INSERT OR", regexp.MustCompile(`(?i)\binsert\s+or\s+(ignore|replace)\b`)},
	{"PRAGMA", regexp.MustCompile(`(?i)\bpragma\s+[a-z_]`)},
	{"IFNULL", regexp.MustCompile(`(?i)\bifnull\s*\(`)},
	{"GLOB", regexp.MustCompile(`(?i)\sglob\s`)},
	// MAX/MIN with two scalar arguments. Portable SQL spells those
	// GREATEST/LEAST, and in Postgres MAX(a, b) is an aggregate arity error.
	{"scalar MAX/MIN", regexp.MustCompile(`(?i)\b(max|min)\(\s*[^(),]+\s*,`)},
}

// Every SQL literal in the package, so the seam cannot be walked around by
// accident and the not-portable list cannot quietly grow.
func packageSQL(t *testing.T) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	looksLikeSQL := regexp.MustCompile(`(?is)^\s*(select|insert|update|delete|with)\s`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := gotoken.NewFileSet()
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			// Raw and interpreted both; only the value matters.
			q := strings.Trim(lit.Value, "`\"")
			if looksLikeSQL.MatchString(q) {
				found[f] = append(found[f], q)
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("found no SQL at all — the scanner is broken, not the code")
	}
	return found
}

func TestEverySQLLiteralSurvivesThePostgresRewrite(t *testing.T) {
	for file, queries := range packageSQL(t) {
		for _, q := range queries {
			got := rewrite(true, q)
			if strings.ContainsRune(stripStrings(got), '?') {
				t.Errorf("%s: a ? survived the rewrite:\n%s", file, got)
			}
			// $1..$n and nothing skipped: a gap means the scan lost count, and
			// Postgres answers a gap with a bind error at runtime.
			for i := 1; strings.Contains(got, "$"+itoa(i)); i++ {
				if i > 64 {
					break
				}
			}
		}
	}
}

func TestNoSQLiteOnlyConstructs(t *testing.T) {
	for file, queries := range packageSQL(t) {
		for _, q := range queries {
			if strings.Contains(q, "sqlite_master") {
				continue // the importer reading a SQLite source on purpose
			}
			for _, bad := range sqliteOnly {
				if bad.re.MatchString(q) {
					t.Errorf("%s: %s does not run on PostgreSQL; write it portably:\n%s",
						file, bad.name, strings.TrimSpace(q))
				}
			}
		}
	}
}

// stripStrings blanks out SQL string literals so a `?` inside one does not read
// as a placeholder the rewrite missed.
func stripStrings(q string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(q); i++ {
		if q[i] == '\'' {
			in = !in
		}
		if in || q[i] == '\'' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
