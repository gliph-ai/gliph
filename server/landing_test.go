package main

import (
	"archive/tar"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The front page goes to a bare / with no session hint, and to nothing else:
// a signed-in reader and an invite link must still land in the app.
func TestLandingOnlyForSignedOutRoot(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "landing.html"), []byte("front"), 0o644)
	landingOn = true
	defer func() { landingOn = false }()

	for _, c := range []struct {
		url    string
		cookie bool
		want   bool
	}{
		{"/", false, true},
		{"/", true, false},
		{"/?invite=abc", false, false},
		{"/signin", false, false},
	} {
		r := httptest.NewRequest("GET", c.url, nil)
		if c.cookie {
			r.AddCookie(&http.Cookie{Name: "gliph_in", Value: "1"})
		}
		if got := landing(httptest.NewRecorder(), r, dir); got != c.want {
			t.Errorf("%s cookie=%v: landing=%v, want %v", c.url, c.cookie, got, c.want)
		}
	}
	landingOn = false
	if landing(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), dir) {
		t.Error("a self-host (GLIPH_LANDING unset) served the front page")
	}
}

func TestInstallBundleHasBinaryAndClient(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("app"), 0o644)
	os.WriteFile(filepath.Join(dir, "landing.html"), []byte("front"), 0o644)
	exe := filepath.Join(t.TempDir(), "gliph")
	os.WriteFile(exe, []byte("bin"), 0o755)
	w := httptest.NewRecorder()
	if err := writeBundle(w, exe, dir); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		got[h.Name] = true
	}
	if !got["gliph"] || !got["web/index.html"] || got["web/landing.html"] {
		t.Errorf("bundle = %v", got)
	}
}
