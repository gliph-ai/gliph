package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func apnsKeyFile(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p := filepath.Join(t.TempDir(), "AuthKey_ABC123DEF4.p8")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	return p, k
}

// One wake to Apple: a background push with an empty payload, a signed
// provider token, and the sandbox tried when production says the token is
// not one of its own.
func TestAPNSSendsContentFreeBackgroundPush(t *testing.T) {
	s := testServer(t)
	path, key := apnsKeyFile(t)
	u, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	s.St.RegisterPushDevice(u.ID, "ios", "devtoken")

	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"reason":"BadDeviceToken"}`)
	}))
	defer prod.Close()
	var got *http.Request
	var body []byte
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
	}))
	defer sandbox.Close()

	a, err := NewAPNSSender(s.St.DeletePushToken, path, "ABC123DEF4", "TEAM123456", "chat.gliph.mobile")
	if err != nil {
		t.Fatal(err)
	}
	a.hosts = []string{prod.URL, sandbox.URL}
	a.wakeAll([]PushDevice{{Platform: "android", Token: "not-me"}, {Platform: "ios", Token: "devtoken"}})

	if got == nil || got.URL.Path != "/3/device/devtoken" {
		t.Fatalf("sandbox never asked for the device: %+v", got)
	}
	if string(body) != `{"aps":{"content-available":1}}` {
		t.Fatalf("payload = %s", body)
	}
	for k, v := range map[string]string{"apns-topic": "chat.gliph.mobile", "apns-push-type": "background", "apns-priority": "5"} {
		if got.Header.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, got.Header.Get(k), v)
		}
	}
	// The bearer is an ES256 JWT this team's key signed.
	tok := strings.TrimPrefix(got.Header.Get("authorization"), "bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("bearer = %q", tok)
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h struct{ Alg, Kid string }
	json.Unmarshal(hdr, &h)
	if h.Alg != "ES256" || h.Kid != "ABC123DEF4" {
		t.Fatalf("header = %s", hdr)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("bearer signature does not verify with the auth key")
	}
	if a.bearer() != tok {
		t.Fatal("a fresh token per push; Apple wants one reused for 20 minutes")
	}
}

// A token Apple has retired is dropped, so the account stops being woken on a
// phone that deleted the app.
func TestAPNSDropsRetiredTokens(t *testing.T) {
	s := testServer(t)
	path, _ := apnsKeyFile(t)
	u, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	s.St.RegisterPushDevice(u.ID, "ios", "gone")
	s.St.RegisterPushDevice(u.ID, "ios", "alive")

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(410)
			io.WriteString(w, `{"reason":"Unregistered"}`)
		}
	}))
	defer gone.Close()
	a, _ := NewAPNSSender(s.St.DeletePushToken, path, "K", "T", "chat.gliph.mobile")
	a.hosts = []string{gone.URL}
	a.wakeAll([]PushDevice{{Platform: "ios", Token: "gone"}, {Platform: "ios", Token: "alive"}})

	left, _ := s.St.PushDevices(u.ID)
	if len(left) != 1 || left[0].Token != "alive" {
		t.Fatalf("devices left = %+v, want only alive", left)
	}
}

// iOS goes to Apple, everything else to whatever else is configured.
func TestPushRoutesByPlatform(t *testing.T) {
	ios, other := &spySender{}, &spySender{}
	byPlatform{"ios": ios, "android": other}.Wake([]PushDevice{
		{Platform: "ios", Token: "i"}, {Platform: "android", Token: "a"}, {Platform: "ios", Token: "j"},
	})
	if len(ios.calls) != 1 || len(ios.calls[0].devices) != 2 || len(other.calls) != 1 || other.calls[0].devices[0].Token != "a" {
		t.Fatalf("ios=%+v other=%+v", ios.calls, other.calls)
	}
}
