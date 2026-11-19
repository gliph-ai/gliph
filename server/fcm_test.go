package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One wake to Google: an OAuth exchange with a signed assertion, then a
// data-only message carrying nothing but wake=1, to Android devices only. A
// token FCM calls UNREGISTERED is dropped.
func TestFCMSendsContentFreeDataWake(t *testing.T) {
	s := testServer(t)
	u, _ := s.St.CreateUser("away", "a@x", "password1", "member")
	s.St.RegisterPushDevice(u.ID, "android", "live")
	s.St.RegisterPushDevice(u.ID, "android", "gone")

	var assertion string
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if v.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", v.Get("grant_type"))
		}
		assertion = v.Get("assertion")
		io.WriteString(w, `{"access_token":"ya29.test","expires_in":3600}`)
	}))
	defer oauth.Close()

	var sent []map[string]any
	fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.test" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var m map[string]map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		sent = append(sent, m["message"])
		if m["message"]["token"] == "gone" {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"status":"UNREGISTERED"}}`)
		}
	}))
	defer fcm.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	cred, _ := json.Marshal(map[string]string{
		"project_id":   "gliph-test",
		"client_email": "push@gliph-test.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":    oauth.URL,
	})
	p := filepath.Join(t.TempDir(), "sa.json")
	os.WriteFile(p, cred, 0o600)

	f, err := NewFCMSender(s.St.DeletePushToken, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.sendURL, "/v1/projects/gliph-test/messages:send") {
		t.Fatalf("sendURL = %s", f.sendURL)
	}
	f.sendURL = fcm.URL
	f.wakeAll([]PushDevice{
		{Platform: "ios", Token: "not-me"},
		{Platform: "android", Token: "live"},
		{Platform: "android", Token: "gone"},
	})

	if len(strings.Split(assertion, ".")) != 3 {
		t.Fatalf("assertion = %q", assertion)
	}
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2 (android only): %v", len(sent), sent)
	}
	for _, m := range sent {
		if _, ok := m["notification"]; ok {
			t.Fatalf("message carries a notification block: %v", m)
		}
		data, _ := m["data"].(map[string]any)
		if len(data) != 1 || data["wake"] != "1" {
			t.Fatalf("data = %v, want only wake=1", data)
		}
	}
	devs, _ := s.St.PushDevices(u.ID)
	if len(devs) != 1 || devs[0].Token != "live" {
		t.Fatalf("devices after UNREGISTERED = %+v", devs)
	}
}
