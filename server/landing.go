package main

// gliph.sh's front page and the self-host installer it advertises.
//
// GLIPH_LANDING=1 turns both on (.pl.yaml). A self-hosted server leaves it
// unset: its visitors are its own team, and they want the sign-in form at /,
// not our pitch.
//
// The session lives in the client's localStorage, which the server cannot see,
// so the client leaves a `gliph_in` cookie as the hint (web/src/session.ts).
// No cookie at a bare / means the front page. An invite link (/?invite=...),
// a ?next= bounce and every other path still get the app.

import (
	"archive/tar"
	"compress/gzip"
	_ "embed"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var landingOn = os.Getenv("GLIPH_LANDING") != ""

//go:embed install.sh
var installScript []byte

// landing serves the front page when this request should see it, and says so.
func landing(w http.ResponseWriter, r *http.Request, webDir string) bool {
	if !landingOn || r.URL.Path != "/" || r.URL.RawQuery != "" || r.Method != http.MethodGet {
		return false
	}
	if c, err := r.Cookie("gliph_in"); err == nil && c.Value == "1" {
		return false
	}
	p := filepath.Join(webDir, "landing.html")
	if _, err := os.Stat(p); err != nil {
		return false
	}
	// The answer depends on the cookie, so no cache may hand one reader's
	// page to the other.
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeFile(w, r, p)
	return true
}

// landingRoutes: GET /install.sh, and the tarball it downloads.
func landingRoutes(mux *http.ServeMux, webDir string) {
	if !landingOn {
		return
	}
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write(installScript)
	})
	// ponytail: the server hands out itself, the binary it is running plus the
	// web client next to it, so only this build's arch (linux/amd64 on gliph.sh)
	// exists here. When arm64 hosts matter, cross-build in the Dockerfile and
	// serve the second binary from the image.
	mux.HandleFunc("GET /install/{file}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("file") != "gliph-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz" {
			http.Error(w, "no build for that platform yet", http.StatusNotFound)
			return
		}
		exe, err := os.Executable()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Cache-Control", "no-cache")
		if err := writeBundle(w, exe, webDir); err != nil {
			// Headers are gone by now; the truncated gzip fails tar on the far end.
			return
		}
	})
}

// writeBundle streams gliph and web/ as a .tar.gz.
func writeBundle(w io.Writer, exe, webDir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(src, name string, mode int64) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	}
	if err := add(exe, "gliph", 0o755); err != nil {
		return err
	}
	if webDir != "" {
		err := filepath.WalkDir(webDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(webDir, p)
			// The front page is gliph.sh's; a self-host has no use for it.
			if strings.HasPrefix(rel, "landing") {
				return nil
			}
			return add(p, "web/"+filepath.ToSlash(rel), 0o644)
		})
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
