package main

// How the server listens. Spec, non-functional: "All transport TLS 1.3; no
// plaintext listener even for localhost admin."
//
// Three modes, and the operator picks one; there is no default, because a
// default would be either a plaintext listener nobody asked for or a
// certificate request to a domain nobody named:
//
//	-tls-domain chat.example.com   Let's Encrypt via autocert, :443, :80 redirects
//	-tls-cert f -tls-key f         the operator's own certificate, :80 redirects
//	-plaintext                     plain HTTP on -addr, ONLY behind a proxy that
//	                               terminates TLS: gliph.sh (Pushleft's edge), a dev box
//
// The TLS modes accept TLS 1.3 and nothing older. Go's 1.3 cipher suites are
// fixed and all AEAD, so there is no suite list to get wrong.

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

type listenOpts struct {
	addr      string // -addr; "" means the mode's default
	httpAddr  string // where plain HTTP redirects to HTTPS (and answers ACME); "" = none
	domains   string // -tls-domain, comma-separated
	cert, key string
	plaintext bool
	dataDir   string // autocert's certificate cache lives in <data>/acme
}

func (o listenOpts) configured() bool {
	return o.plaintext || o.domains != "" || o.cert != ""
}

// listen starts the listeners and returns them, so main can close them on a
// signal. A listener that fails to start kills the process: a server that is
// up on no port is worse than one that says why it is not.
func listen(h http.Handler, o listenOpts) []*http.Server {
	run := func(s *http.Server, f func() error) {
		go func() {
			if err := f(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen %s: %v", s.Addr, err)
			}
		}()
	}
	if o.plaintext {
		s := &http.Server{Addr: or(o.addr, ":8787"), Handler: h, ReadHeaderTimeout: 10 * time.Second}
		log.Printf("plaintext HTTP on %s: run this only behind a proxy that terminates TLS", s.Addr)
		run(s, s.ListenAndServe)
		return []*http.Server{s}
	}
	s, redirect := tlsServers(h, o)
	run(s, func() error { return s.ListenAndServeTLS(o.cert, o.key) })
	out := []*http.Server{s}
	if redirect != nil {
		// Not fatal: without root or CAP_NET_BIND_SERVICE port 80 is out of
		// reach, and HTTPS on its own still serves. autocert falls back to the
		// TLS-ALPN challenge on :443.
		go func() {
			if err := redirect.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("warning: no HTTP→HTTPS redirect on %s: %v", redirect.Addr, err)
			}
		}()
		out = append(out, redirect)
	}
	log.Printf("TLS 1.3 on %s", s.Addr)
	return out
}

// tlsServers builds the HTTPS server and its plain-HTTP redirector (nil when
// -http-addr is empty). Separate from listen so a test can serve them on a
// loopback port.
func tlsServers(h http.Handler, o listenOpts) (*http.Server, *http.Server) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	var redirect http.Handler = redirectHTTPS(or(o.addr, ":443"))
	if o.domains != "" {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(splitList(o.domains)...),
			Cache:      autocert.DirCache(filepath.Join(o.dataDir, "acme")),
		}
		cfg = m.TLSConfig()
		cfg.MinVersion = tls.VersionTLS13
		redirect = m.HTTPHandler(redirect) // answers the HTTP-01 challenge, redirects the rest
	}
	s := &http.Server{Addr: or(o.addr, ":443"), Handler: hsts(h), TLSConfig: cfg,
		ReadHeaderTimeout: 10 * time.Second}
	if o.httpAddr == "" {
		return s, nil
	}
	return s, &http.Server{Addr: o.httpAddr, Handler: redirect, ReadHeaderTimeout: 10 * time.Second}
}

// redirectHTTPS sends every plain-HTTP request to the same path over HTTPS.
func redirectHTTPS(httpsAddr string) http.Handler {
	port := ""
	if _, p, err := net.SplitHostPort(httpsAddr); err == nil && p != "443" {
		port = ":" + p
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+hostKey(r.Host)+port+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// hsts tells a browser that came once over HTTPS never to try plain HTTP again.
func hsts(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		h.ServeHTTP(w, r)
	})
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
