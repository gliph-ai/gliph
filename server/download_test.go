package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two traps, both of which produce a release that looks published and silently
// never reaches anyone.
func TestUpdaterManifestIsFreshAndPicksTheRealNewest(t *testing.T) {
	dir := t.TempDir()
	// 0.9.0 sorts AFTER 0.10.0 as a string, which is how a version-sorting bug
	// ships an old manifest forever. 0.10.1-beta.2 is genuinely newer by
	// SemVer and must STILL not be served here: this is the stable channel.
	for _, v := range []string{"0.9.0", "0.10.0", "0.10.1-beta.2", "notaversion"} {
		if err := os.MkdirAll(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, v, "latest.json"),
			[]byte(`{"version":"`+v+`"}`), 0o644)
	}

	if got := newestRelease(dir); got != "0.10.0" {
		t.Fatalf("newest release is %q, want 0.10.0 (a prerelease must never be served on the stable channel, and 0.9 must not outrank 0.10)", got)
	}

	s := testServer(t)
	s.DownloadDir = dir
	h := s.Routes("", t.TempDir())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/latest.json", nil))
	if w.Code != 200 {
		t.Fatalf("manifest: %d", w.Code)
	}
	if body := w.Body.String(); body != `{"version":"0.10.0"}` {
		t.Fatalf("served the wrong manifest: %s", body)
	}
	// The whole reason this is a handler and not a static file.
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("manifest Cache-Control is %q — a long TTL here makes every "+
			"release after the first invisible", cc)
	}

	// A versioned artifact is immutable and should say so.
	os.WriteFile(filepath.Join(dir, "0.10.0", "Gliph.app.tar.gz"), []byte("bytes"), 0o644)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/0.10.0/Gliph.app.tar.gz", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") == "public, max-age=60" {
		t.Fatalf("artifact: %d cache=%q", w.Code, w.Header().Get("Cache-Control"))
	}
}

// Nothing published is a 404, not a panic or an empty manifest the updater
// would try to parse.
func TestNoReleasesIsAnHonest404(t *testing.T) {
	s := testServer(t)
	s.DownloadDir = t.TempDir()
	h := s.Routes("", t.TempDir())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/latest.json", nil))
	if w.Code != 404 {
		t.Fatalf("empty download dir returned %d, want 404", w.Code)
	}
}

func TestDownloadsPageListsInstallersOfTheNewestRelease(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "0.2.0"), 0o755)
	for _, f := range []string{"Gliph_0.2.0_aarch64.dmg", "Gliph_0.2.0_x64-setup.exe",
		"Gliph_0.2.0_amd64.AppImage", "Gliph_0.1.9_x64-setup.exe", "Gliph.app.tar.gz", "Gliph.app.tar.gz.sig", "latest.json"} {
		os.WriteFile(filepath.Join(dir, "0.2.0", f), []byte("x"), 0o644)
	}
	s := testServer(t)
	s.DownloadDir = dir
	h := s.RoutesWith("", t.TempDir(), dir)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/", nil))
	body := w.Body.String()
	for _, want := range []string{"0.2.0/Gliph_0.2.0_aarch64.dmg", "0.2.0/Gliph_0.2.0_x64-setup.exe", "0.2.0/Gliph_0.2.0_amd64.AppImage"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	// The updater's own files are not for humans, and a stray from another
	// version is not this release.
	if strings.Contains(body, "tar.gz") || strings.Contains(body, ".sig") || strings.Contains(body, "0.1.9") {
		t.Errorf("page lists updater artifacts or a stray version")
	}
	// The per-platform button: a stable URL that lands on this release's file.
	for os, want := range map[string]string{"mac": "aarch64.dmg", "windows": "x64-setup.exe", "linux": "amd64.AppImage"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/latest/"+os, nil))
		if loc := w.Header().Get("Location"); w.Code != 302 || !strings.HasPrefix(loc, "/downloads/desktop/0.2.0/") || !strings.HasSuffix(loc, want) {
			t.Errorf("latest/%s: %d %q, want a 302 to 0.2.0's %s", os, w.Code, loc, want)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/downloads/desktop/latest/ios", nil))
	if w.Code != 404 {
		t.Errorf("latest/ios: %d, want 404", w.Code)
	}
}
