package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	return
}

// The TLS listener speaks 1.3 and nothing older, sets HSTS, and the plain
// HTTP side only redirects.
func TestTLSListenerIsTLS13Only(t *testing.T) {
	cert, key := selfSigned(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) })
	s, redirect := tlsServers(ok, listenOpts{addr: ":8443", httpAddr: ":8080", cert: cert, key: key})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.ServeTLS(ln, cert, key)
	defer s.Close()

	dial := func(max uint16) (*http.Response, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, MaxVersion: max}}}
		return c.Get("https://" + ln.Addr().String() + "/")
	}
	if _, err := dial(tls.VersionTLS12); err == nil {
		t.Fatal("a TLS 1.2 client was served")
	}
	resp, err := dial(tls.VersionTLS13)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS.Version != tls.VersionTLS13 || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("version %x, hsts %q", resp.TLS.Version, resp.Header.Get("Strict-Transport-Security"))
	}

	rec := httptest.NewRecorder()
	redirect.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://chat.example.com/admin?x=1", nil))
	if rec.Code != 301 || rec.Header().Get("Location") != "https://chat.example.com:8443/admin?x=1" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Let's Encrypt mode: the same floor, and the HTTP side still redirects.
func TestAutocertListenerIsTLS13Only(t *testing.T) {
	s, redirect := tlsServers(http.NotFoundHandler(), listenOpts{httpAddr: ":80", domains: "chat.example.com", dataDir: t.TempDir()})
	if s.Addr != ":443" || s.TLSConfig.MinVersion != tls.VersionTLS13 || s.TLSConfig.GetCertificate == nil {
		t.Fatalf("addr %s, min %x", s.Addr, s.TLSConfig.MinVersion)
	}
	rec := httptest.NewRecorder()
	redirect.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://chat.example.com/", nil))
	if rec.Code != 302 && rec.Code != 301 || rec.Header().Get("Location") != "https://chat.example.com/" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
