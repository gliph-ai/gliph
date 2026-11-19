package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) authUser(r *http.Request) (*User, error) {
	t := r.Header.Get("Authorization")
	t = strings.TrimPrefix(t, "Bearer ")
	if t == "" {
		t = r.URL.Query().Get("token")
	}
	if t == "" {
		return nil, errors.New("no token")
	}
	return s.St.UserByToken(t)
}

func (s *Server) Routes(webDir, uploadDir string) http.Handler {
	return s.RoutesWith(webDir, uploadDir, s.DownloadDir)
}

func (s *Server) RoutesWith(webDir, uploadDir, downloadDir string) http.Handler {
	mux := http.NewServeMux()
	if s.Files == nil {
		// uploads/ and roomnotes/ are siblings under the data dir.
		s.Files = diskFiles(filepath.Dir(uploadDir))
	}

	// ---- auth ----

	mux.HandleFunc("POST /api/register", func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Nick, Email, Password, Invite string }
		json.NewDecoder(r.Body).Decode(&d)
		// One transaction decides all three things: whether this is the account
		// that owns the server, whether the invite is still free, and whether the
		// account can be created. A failure anywhere spends nothing.
		if _, err := s.St.Register(d.Nick, d.Email, d.Password, d.Invite); err != nil {
			if errors.Is(err, ErrBadInvite) || errors.Is(err, ErrUsedInvite) {
				fail(w, 403, err.Error())
				return
			}
			fail(w, 400, err.Error())
			return
		}
		tok, u2, err := s.St.Login(d.Nick, d.Password)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		// everyone lands in #general
		if ch, err := s.St.ChannelByName("general"); err == nil {
			s.St.Join(ch.ID, u2.ID, "member")
			// meta.new marks a first arrival on the server, not a /join — it is the
			// one join every member gets told about (core announce()).
			s.Post(ch, &Event{Kind: "join", Actor: u2.Nick, Body: "joined the server", Meta: map[string]any{"new": true}}, "")
			s.PushChannelState(ch.ID)
		}
		writeJSON(w, 200, map[string]any{"token": tok, "user": u2})
	})

	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Nick, Password string }
		json.NewDecoder(r.Body).Decode(&d)
		if !s.loginAllowed(r, d.Nick) {
			fail(w, 429, "too many login attempts — wait a moment")
			return
		}
		tok, u, err := s.St.Login(d.Nick, d.Password)
		if err != nil {
			fail(w, 401, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"token": tok, "user": u})
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.St.Logout(t)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		// The switcher polls this on every server it is NOT looking at, so the
		// answer carries exactly what a bracket in the top bar needs: who you
		// are HERE, what this server calls itself, and how much is waiting.
		// The account fields keep their old names and shape — the console and
		// the device-link page read them.
		writeJSON(w, 200, map[string]any{
			"id":     u.ID,
			"nick":   u.Nick,
			"role":   u.Role,
			"away":   u.Away,
			"server": s.Name,
			"icon":   s.St.Setting(settingIcon),
			"unread": s.St.UnreadFor(u.ID),
		})
	})

	// ---- admin ----

	// The Authorization header, and only that. authUser also accepts ?token= for
	// the websocket handshake, where a browser cannot set a header — but a token
	// in a URL is a token in the proxy's access log, the browser history and any
	// Referer that leaks off the page, and the admin token is the one on this
	// server that is worth stealing. The console and Prometheus both send a
	// header, so nothing legitimate needs the query form here.
	admin := func(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			u, err := s.St.UserByToken(t)
			// One answer for "no token", "bad token" and "not an admin": telling
			// them apart turns this into an oracle for which tokens are real.
			if t == "" || err != nil || u.Role != "admin" {
				fail(w, 403, "admin only")
				return
			}
			h(w, r, u)
		}
	}

	// admin console: users, channels, uploads, stats (see admin.go)
	s.adminRoutes(mux, admin)
	s.DevlogRoutes(mux) // POST /api/hooks/git/{token} — see devlog.go

	// ---- uploads: offset-based resume ----
	// ponytail: ~60 lines instead of embedding tusd. Same create/PATCH/offset
	// contract; swap in tusd if S3 or the full tus extension set is needed.

	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		if !s.Limits.Uploads.Allow(u.ID) {
			fail(w, 429, errTooFast.Error())
			return
		}
		var d struct {
			Channel string `json:"channel"`
			Name    string `json:"name"`
			Size    int64  `json:"size"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		if d.Size <= 0 || d.Size > s.UploadMax {
			fail(w, 413, fmt.Sprintf("size must be between 1 and %d bytes", s.UploadMax))
			return
		}
		if _, ok := s.St.MemberRole(d.Channel, u.ID); !ok {
			fail(w, 403, "not a member of that channel")
			return
		}
		up, err := s.St.CreateUpload(d.Channel, u.ID, u.Nick, filepath.Base(d.Name), d.Size, "uploads/"+id("blob"), s.UploadTTL)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": up.ID, "offset": 0, "expires_at": up.Expires})
	})

	// Authenticated like every other upload route. This one used to be open,
	// which made it a free oracle for "does this upload id exist, and how many
	// bytes are in it" — the resume probe is only ever asked by the uploader.
	mux.HandleFunc("HEAD /api/upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		up, err := s.St.Upload(r.PathValue("id"))
		if err != nil || up.OwnerID != u.ID {
			fail(w, 404, "no such upload")
			return
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(up.Received, 10))
		w.WriteHeader(200)
	})

	mux.HandleFunc("PATCH /api/upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		up, err := s.St.Upload(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such upload")
			return
		}
		// By id, never by nick. Ownership used to be a nick comparison, and
		// /nick renames freely — so releasing a nick handed an in-flight upload
		// to whoever claimed it next. A pre-migration row has an empty OwnerID
		// and so matches nobody, which is the right way to fail.
		if up.OwnerID != u.ID {
			fail(w, 403, "not your upload")
			return
		}
		off, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		if off != up.Received {
			w.Header().Set("Upload-Offset", strconv.FormatInt(up.Received, 10))
			fail(w, 409, "offset mismatch")
			return
		}
		// A chunk is stored whole or not at all, so its length has to be known
		// up front: an S3 PUT needs it, and a half-written chunk would move the
		// offset past bytes nobody can vouch for.
		n := min(r.ContentLength, up.Size-off)
		if r.ContentLength < 0 {
			fail(w, 411, "Content-Length required")
			return
		}
		if err := s.Files.write(up.Path, off, r.Body, n); err != nil {
			fail(w, 500, "chunk not stored; resume from Upload-Offset")
			return
		}
		received := off + n
		complete := received >= up.Size
		s.St.UpdateUpload(up.ID, received, complete)
		if complete {
			if ch, err := s.St.ChannelByID(up.Channel); err == nil {
				s.Post(ch, &Event{Kind: "file", Actor: u.Nick, Body: up.Name,
					Meta: map[string]any{"file_id": up.ID, "size": up.Size, "expires_at": up.Expires}}, "")
			}
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(received, 10))
		writeJSON(w, 200, map[string]any{"offset": received, "complete": complete})
	})

	mux.HandleFunc("GET /api/file/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		up, err := s.St.Upload(r.PathValue("id"))
		if err != nil || !up.Complete {
			fail(w, 404, "no such file")
			return
		}
		if _, ok := s.St.MemberRole(up.Channel, u.ID); !ok {
			fail(w, 403, "not a member of that channel")
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(up.Name))
		w.Header().Set("Content-Type", "application/octet-stream")
		s.Files.serve(w, r, up.Path)
	})

	// ---- websockets ----

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(1 << 20)
		s.serveConn(r.Context(), ws, u, r.URL.Query().Get("client"))
	})

	mux.HandleFunc("/plugin", func(w http.ResponseWriter, r *http.Request) {
		t := r.URL.Query().Get("token")
		if t == "" {
			t = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		p, err := s.St.PluginByToken(t)
		if err != nil {
			fail(w, 401, "unknown plugin token")
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(1 << 20)
		s.servePlugin(r.Context(), ws, p)
	})

	s.pushRoutes(mux)
	s.deviceLinkRoutes(mux)
	s.iceRoutes(mux)
	// 3D room boards + voice notes; the audio lives beside the uploads dir.
	s.roomRoutes(mux)
	s.downloadRoutes(mux, downloadDir)

	// The whole changelog, oldest first — the same notes clients see in the
	// stream, for a website, a changelog page or a curl. Public on purpose:
	// these are the product's own announcements, and there is nothing in them
	// that is not already on every client.
	mux.HandleFunc("GET /api/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSON(w, 200, releases)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "server": s.Name})
	})

	// /metrics needs an admin token. A localhost-only bind would be a lie behind
	// the reverse proxy every self-host runs (every request looks like 127.0.0.1);
	// Prometheus reads a bearer token from a file, so this costs an operator one
	// scrape-config line and cannot be misconfigured open.
	mux.HandleFunc("GET /metrics", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.writeMetrics(w)
	}))

	// ---- static web client (SPA fallback) ----

	if webDir != "" {
		fs := http.FileServer(http.Dir(webDir))

		// The console is its own SPA at /admin, with /admin/users, /admin/stats
		// and friends as real URLs an operator can bookmark and reload. Both
		// patterns are needed: "/admin/" alone does not match a bare "/admin",
		// and ServeMux would 301 it into a redirect the deep links don't want.
		adminPage := func(w http.ResponseWriter, r *http.Request) {
			// frame-ancestors has to come from a header — browsers ignore it in
			// the page's own meta CSP, which is where the rest of the console's
			// policy lives. Nothing may embed a page of one-click delete buttons.
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.ServeFile(w, r, filepath.Join(webDir, "admin.html"))
		}
		mux.HandleFunc("GET /admin", adminPage)
		mux.HandleFunc("GET /admin/", adminPage)
		landingRoutes(mux, webDir)

		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if landing(w, r, webDir) { // gliph.sh's front page, landing.go
				return
			}
			// Clean the path ANCHORED AT ROOT, the way http.Dir does before it
			// opens anything: "/../../etc/passwd" collapses to "/etc/passwd"
			// instead of climbing out of webDir. Plain filepath.Clean does not
			// stop the climb — filepath.Join would then resolve a path outside
			// webDir, and this Stat would answer for a file the FileServer below
			// will (correctly) refuse to serve. Nothing leaked either way; the
			// two just disagreed, which is how a real traversal gets missed.
			p := filepath.Join(webDir, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				fs.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
		})
	}

	return cors(mux)
}

// cors keeps the Tauri and mobile shells (which load from their own origins)
// able to talk to a self-hosted server.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Upload-Offset")
		// DELETE is in here because the admin console uses it (revoke an invite,
		// a plugin, an upload, an account). Without it those four buttons fail
		// preflight from any shell that is not same-origin with the server.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "Upload-Offset")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// downloadRoutes serves desktop release artifacts and the updater manifest.
//
// THE ONE THING THAT MATTERS HERE: `latest.json` is served from a handler with
// a SHORT cache lifetime, never as a plain static file. Everything else under
// a downloads directory is content-addressed by version and wants to be cached
// forever — and the moment you tell a CDN or a browser that, a static
// `latest.json` inherits the same year-long TTL and every release after the
// first becomes invisible. The manifest is the one file that has to stay warm.
//
// The bytes are served with no authentication: they are signed installers whose
// updater signature is what makes them trustworthy, and requiring a login to
// download an update would break updating for exactly the people whose session
// has expired.
func (s *Server) downloadRoutes(mux *http.ServeMux, dir string) {
	if dir == "" {
		return
	}
	rel := s.releaseSource(dir)

	mux.HandleFunc("GET /downloads/desktop/latest.json", func(w http.ResponseWriter, r *http.Request) {
		v := rel.newest()
		if v == "" {
			http.Error(w, "no release published", 404)
			return
		}
		body, err := rel.read(v + "/latest.json")
		if err != nil {
			http.Error(w, "no manifest for "+v, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write(body)
	})

	// The human page: one link per installer of the newest release, for a
	// tester's first install. Everything after that first install is the
	// updater's job, so this page never needs a version list.
	// Both spellings: /download is what people type.
	for _, p := range []string{"GET /download", "GET /downloads"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/downloads/desktop/", http.StatusFound)
		})
	}
	mux.HandleFunc("GET /downloads/desktop/{$}", func(w http.ResponseWriter, r *http.Request) {
		v := rel.newest()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		downloadsPage.Execute(w, map[string]any{"Name": s.Name, "Version": v, "Links": installers(rel.names(v), v)})
	})

	// One stable URL per platform that lands on the newest installer, so a
	// client can put "get Gliph for macOS" on a button and have the click be
	// the download rather than a page with the download on it.
	mux.HandleFunc("GET /downloads/desktop/latest/{os}", func(w http.ResponseWriter, r *http.Request) {
		v := rel.newest()
		for _, in := range installers(rel.names(v), v) {
			if in.OS == r.PathValue("os") {
				w.Header().Set("Cache-Control", "public, max-age=60")
				http.Redirect(w, r, "/downloads/desktop/"+v+"/"+in.File, http.StatusFound)
				return
			}
		}
		http.NotFound(w, r)
	})

	// Versioned artifacts are immutable: a given version's bytes never change,
	// so anything that caches them can keep them.
	mux.Handle("GET /downloads/desktop/", http.StripPrefix("/downloads/desktop/",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			rel.serve(w, r)
		})))
}

// releaseSource is where published releases live: a directory, or with
// "s3:<prefix>" that prefix of the files bucket (gliph.sh, which has no disk
// that outlives a deploy). In the bucket, <prefix>/latest.json is written last
// by bin/release-desktop and names the newest version: the publish point,
// the way the newest directory is on disk.
type releaseSource struct {
	newest func() string
	read   func(path string) ([]byte, error)
	names  func(v string) []string
	serve  http.HandlerFunc // path relative to the release root
}

func (s *Server) releaseSource(dir string) releaseSource {
	if prefix, ok := strings.CutPrefix(dir, "s3:"); ok {
		b, _ := s.Files.(*s3Files)
		if b == nil {
			log.Fatal("-downloads s3:… needs the S3 bucket (S3_BUCKET and friends)")
		}
		b = b.under("") // releases are the shard's, not a team's
		prefix = strings.Trim(prefix, "/") + "/"
		return releaseSource{
			newest: func() string {
				var m struct{ Version string }
				body, err := b.get(prefix + "latest.json")
				if err != nil || json.Unmarshal(body, &m) != nil || semverParts(m.Version) == nil {
					return ""
				}
				return m.Version
			},
			read: func(p string) ([]byte, error) { return b.get(prefix + p) },
			names: func(v string) []string {
				objs, _ := b.list(prefix + v + "/")
				var out []string
				for _, o := range objs {
					out = append(out, path.Base(o.Key))
				}
				return out
			},
			serve: func(w http.ResponseWriter, r *http.Request) {
				p := path.Clean("/" + r.URL.Path)[1:]
				if p == "" || strings.HasSuffix(r.URL.Path, "/") {
					http.NotFound(w, r)
					return
				}
				b.serveObject(w, r, prefix+p)
			},
		}
	}
	fs := http.FileServer(http.Dir(dir))
	return releaseSource{
		newest: func() string { return newestRelease(dir) },
		read:   func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))) },
		names: func(v string) []string {
			var out []string
			if v == "" {
				return out
			}
			entries, _ := os.ReadDir(filepath.Join(dir, v))
			for _, e := range entries {
				out = append(out, e.Name())
			}
			return out
		},
		serve: fs.ServeHTTP,
	}
}

type installer struct{ OS, Label, Note, File string }

// installers lists release v's human-facing bundles, in listing order.
func installers(names []string, v string) []installer {
	var out []installer
	if v == "" {
		return out
	}
	for _, n := range names {
		// A bundle names its version (Gliph_0.1.62_x64-setup.exe); one
		// naming another version is a stray from an earlier build.
		if strings.Contains(n, "_") && !strings.Contains(n, "_"+v+"_") {
			continue
		}
		if in := installerFor(n); in != nil {
			out = append(out, *in)
		}
	}
	return out
}

// installerFor names the OS a bundle installs on, or nil for files that are the
// updater's business (the .app.tar.gz, every .sig, the manifest).
func installerFor(name string) *installer {
	switch {
	case strings.HasSuffix(name, ".dmg"):
		return &installer{"mac", "macOS", "Apple silicon · open the .dmg and drag Gliph to Applications", name}
	case strings.HasSuffix(name, "-setup.exe"):
		return &installer{"windows", "Windows", "64-bit · run the installer", name}
	case strings.HasSuffix(name, ".AppImage"):
		return &installer{"linux", "Linux", "AppImage · make it executable and run it", name}
	case strings.HasSuffix(name, ".deb"):
		return &installer{"linux", "Linux", "Debian / Ubuntu · sudo apt install ./" + name, name}
	}
	return nil
}

// The download page dresses like the admin console (web/src/admin.css): Inter,
// sentence case, one 18px card with a tinted border, pill buttons, the creme
// palette. Inline rather than a file in web/ because it is server-rendered from
// the release directory and has no client code to share.
var downloadsPage = template.Must(template.New("downloads").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#1a2138">
<title>gliph · downloads</title>
<link rel="stylesheet" href="/fonts/g/fonts.css">
<style>
  :root{--chrome:#1a2138;--chrome-text:#f5f1e8;--chrome-dim:#9aa3c4;--bg:#e9e3d2;--surface:#f5f1e8;--surface-2:#ece7d8;
    --text:#23262e;--text-2:#383c4a;--muted:#a29c88;--border:#c2bba6;--accent:#d94f75;--azure:#5b86d6}
  *{box-sizing:border-box}
  body{margin:0;min-height:100dvh;background:var(--bg);color:var(--text);
    font:15px/1.5 Inter,ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;letter-spacing:-.1px}
  header{background:var(--chrome);color:var(--chrome-dim);padding:22px 24px;display:flex;align-items:baseline;gap:14px}
  .wordmark{font:400 30px/1 "Abril Fatface",Georgia,serif;color:var(--accent);text-decoration:none}
  .sub{font-size:12px;font-weight:600;letter-spacing:.09em;text-transform:uppercase;opacity:.65}
  main{max-width:640px;margin:0 auto;padding:28px 20px 60px}
  h1{font-size:26px;font-weight:650;letter-spacing:-.4px;margin:0 0 6px}
  .lead{color:var(--text-2);margin:0 0 20px}
  .card{background:var(--surface);border:1.5px solid var(--border);border-radius:18px;overflow:hidden}
  .row{display:flex;align-items:center;gap:16px;padding:16px 18px;border-top:1px solid var(--border)}
  .row:first-child{border-top:0}
  .row.mine{background:var(--surface-2)}
  .ico{flex:0 0 40px;width:40px;height:40px;border-radius:12px;background:var(--chrome);color:var(--chrome-text);
    display:grid;place-items:center}
  .ico svg{width:22px;height:22px;fill:currentColor}
  .what{flex:1;min-width:0}
  .what b{display:block;font-weight:650;font-size:16px}
  .what small{display:block;color:var(--muted);font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .what .mono{font-family:"JetBrains Mono",ui-monospace,Menlo,monospace;letter-spacing:0}
  .mine .what b::after{content:"for this device";font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;
    color:var(--surface);background:var(--accent);border-radius:9999px;padding:2px 8px;margin-left:8px;vertical-align:2px}
  .pri{flex:0 0 auto;font:inherit;font-size:15px;font-weight:600;padding:10px 20px;line-height:1.2;border:1px solid var(--azure);
    border-radius:9999px;background:var(--azure);color:var(--surface);text-decoration:none;
    box-shadow:inset 0 1px 0 rgb(255 255 255 / 22%);transition:transform .07s ease,background .18s ease}
  .pri:hover{background:color-mix(in srgb,var(--azure) 82%,var(--chrome))}
  .pri:active{transform:scale(.965)}
  .empty{padding:28px 18px;color:var(--muted);text-align:center}
  .after{margin:22px 4px 0;color:var(--text-2);font-size:14px}
  .after b{color:var(--text)}
  @media (max-width:520px){
    header{padding:16px 18px}.wordmark{font-size:24px}
    main{padding:20px 12px 40px}h1{font-size:22px}
    .row{flex-wrap:wrap;padding:14px}
    .what{flex-basis:calc(100% - 56px)}
    .pri{flex-basis:100%;text-align:center;padding:12px 20px}
  }
</style>
<header><a class="wordmark" href="/">gliph</a><span class="sub">downloads</span></header>
<main>
  <h1>Get Gliph for your desktop</h1>
  <p class="lead">{{if .Version}}Version {{.Version}} · from {{.Name}}{{else}}Nothing has been published yet.{{end}}</p>
  <div class="card">
    {{range .Links}}<div class="row" data-os="{{.OS}}">
      <span class="ico">{{if eq .OS "mac"}}<svg viewBox="0 0 24 24"><path d="M16.4 12.7c0-2.6 2.1-3.8 2.2-3.9-1.2-1.8-3.1-2-3.7-2-1.6-.2-3.1.9-3.9.9-.8 0-2-.9-3.4-.9-1.7 0-3.3 1-4.2 2.6-1.8 3.1-.5 7.8 1.3 10.3.9 1.2 1.9 2.6 3.2 2.6 1.3-.1 1.8-.8 3.3-.8s2 .8 3.4.8c1.4 0 2.3-1.3 3.1-2.5 1-1.4 1.4-2.8 1.4-2.9-.1 0-2.7-1-2.7-4.2zM13.9 5.1c.7-.9 1.2-2 1-3.1-1 0-2.2.7-2.9 1.5-.6.7-1.2 1.9-1.1 3 1.1.1 2.3-.6 3-1.4z"/></svg>{{else if eq .OS "windows"}}<svg viewBox="0 0 24 24"><path d="M3 5.5l7.4-1v7H3zM11.4 4.3L21 3v8.5h-9.6zM3 12.5h7.4v7L3 18.5zM11.4 12.5H21V21l-9.6-1.3z"/></svg>{{else}}<svg viewBox="0 0 24 24"><path d="M12 2c-2.6 0-4.2 2.1-4.2 4.8 0 1.2.2 2 0 2.7-.4 1.3-2.2 3.1-2.9 5.1-.4 1.2-.3 2.3.1 3.2-.7.3-1.4.8-1.4 1.5 0 1 1.5 1.3 2.6 1.6.9.3 1.7 1.1 2.8 1.1.7 0 1.2-.4 1.5-.9h2.9c.3.5.8.9 1.5.9 1.1 0 1.9-.8 2.8-1.1 1.1-.3 2.6-.6 2.6-1.6 0-.7-.7-1.2-1.4-1.5.4-.9.5-2 .1-3.2-.7-2-2.5-3.8-2.9-5.1-.2-.7 0-1.5 0-2.7C16.2 4.1 14.6 2 12 2zm-1.6 4.2c.5 0 .8.6.8 1.3s-.3 1.2-.8 1.2-.8-.5-.8-1.2.3-1.3.8-1.3zm3.2 0c.5 0 .8.6.8 1.3s-.3 1.2-.8 1.2-.8-.5-.8-1.2.3-1.3.8-1.3zM12 9c.9 0 2.2.5 2.2 1.1 0 .5-1.3 1.4-2.2 1.4s-2.2-.9-2.2-1.4c0-.6 1.3-1.1 2.2-1.1zm0 3.4c1.9 0 3.4 2.3 3.4 4.4 0 1.5-.7 2.6-1.5 3.1h-3.8c-.8-.5-1.5-1.6-1.5-3.1 0-2.1 1.5-4.4 3.4-4.4z"/></svg>{{end}}</span>
      <span class="what"><b>{{.Label}}</b><small>{{.Note}}</small><small class="mono">{{.File}}</small></span>
      <a class="pri" href="{{$.Version}}/{{.File}}" download>Download</a>
    </div>
    {{else}}<div class="empty">No installers here yet.</div>{{end}}
  </div>
  <p class="after"><b>Install once.</b> Gliph checks for new builds itself and grows a <b>★ update</b> button in its menubar when one is out.</p>
</main>
<script>
  // Mark the visitor's own platform. A guess from the user agent, so it only
  // decorates: every row stays there to download for another machine.
  var p = navigator.userAgent, os = /Windows/.test(p) ? "windows" : /Mac|iPhone|iPad/.test(p) ? "mac" : /Linux|X11/.test(p) ? "linux" : "";
  var row = os && document.querySelector('.row[data-os="' + os + '"]');
  if (row) { row.classList.add("mine"); row.parentNode.prepend(row); }
</script>
`))

// newestRelease picks the highest published version directory. Directories are
// named by SemVer, which does not sort lexically (0.10.0 < 0.9.0 as strings),
// so compare the numbers.
func newestRelease(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestParts := "", []int(nil)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		parts := semverParts(e.Name())
		if parts == nil {
			continue
		}
		if bestParts == nil || semverLess(bestParts, parts) {
			best, bestParts = e.Name(), parts
		}
	}
	return best
}

// semverParts returns [major, minor, patch], or nil for anything that is not a
// plain release version.
//
// A PRERELEASE IS NOT A RELEASE. `0.10.1-beta.2` is genuinely newer than
// `0.10.0` by SemVer, and offering it here would push every beta to everyone on
// the stable channel — so this endpoint does not rank prereleases, it ignores
// them. A beta channel, if it is ever wanted, is a second endpoint with its own
// answer to "newest", not a comparison rule smuggled into this one.
func semverParts(v string) []int {
	if strings.Contains(v, "-") {
		return nil
	}
	bits := strings.Split(v, ".")
	if len(bits) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, b := range bits {
		n, err := strconv.Atoi(b)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func semverLess(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
