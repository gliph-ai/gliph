package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A server with no credentials must not pretend to send. Callers key off this
// to skip building digests nothing will deliver.
func TestUnconfiguredServerHasNoMailer(t *testing.T) {
	m, err := NewSESMailer("", "", "eu-west-1", "")
	if err != nil {
		t.Fatalf("empty credentials should be a normal startup path, got %v", err)
	}
	if MailerConfigured(m) {
		t.Fatal("a server with no credentials reports itself as configured")
	}
	if err := m.Send("someone@example.com", "s", "b"); !errors.Is(err, ErrNoMailer) {
		t.Fatalf("want ErrNoMailer, got %v", err)
	}
}

// Half a credential, or a missing From, is a deployment mistake. Startup is the
// right place to find out — the alternative is discovering it at the first
// digest, days later, with no one watching.
func TestPartialSESConfigFailsAtStartup(t *testing.T) {
	for _, c := range []struct{ name, id, secret, from string }{
		{"id without secret", "AKIA", "", "d@example.com"},
		{"secret without id", "", "shh", "d@example.com"},
		{"no from address", "AKIA", "shh", ""},
		{"unparseable from", "AKIA", "shh", "not an address"},
	} {
		if _, err := NewSESMailer(c.id, c.secret, "eu-west-1", c.from); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
	}
}

// The wire format: what SES receives is the subject and body it was given, from
// the configured identity, signed with a well-formed SigV4 header.
func TestSESSendsSignedRequestWithTheGivenContent(t *testing.T) {
	var gotAuth, gotDate, gotType string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("X-Amz-Date")
		gotType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.WriteHeader(200)
		w.Write([]byte(`{"MessageId":"abc"}`))
	}))
	defer srv.Close()

	m := &sesMailer{
		keyID: "AKIAEXAMPLE", secret: "secret", region: "eu-west-1",
		from: "gliph@example.com", endpoint: srv.URL, client: srv.Client(),
	}
	if err := m.Send("her@example.com", "you missed 12 messages", "in #general and 2 DMs"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
		t.Errorf("Authorization does not carry the key id: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "/eu-west-1/ses/aws4_request") {
		t.Errorf("Authorization scope is not the SES region scope: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=content-type;host;x-amz-date") {
		t.Errorf("signed headers must be lowercase and sorted: %q", gotAuth)
	}
	// A signature that is missing, empty or not hex means the derivation broke.
	_, sig, _ := strings.Cut(gotAuth, "Signature=")
	if len(sig) != 64 || strings.ContainsAny(sig, "GHIJKLMNOPQRSTUVWXYZ ") {
		t.Errorf("signature is not a 64-char hex digest: %q", sig)
	}
	if gotType != "application/json" {
		t.Errorf("content type %q", gotType)
	}
	if _, err := time.Parse("20060102T150405Z", gotDate); err != nil {
		t.Errorf("X-Amz-Date %q is not in SigV4 basic format: %v", gotDate, err)
	}

	if got := body["FromEmailAddress"]; got != "gliph@example.com" {
		t.Errorf("From is %v, want the configured identity", got)
	}
	simple := body["Content"].(map[string]any)["Simple"].(map[string]any)
	if got := simple["Subject"].(map[string]any)["Data"]; got != "you missed 12 messages" {
		t.Errorf("subject is %v", got)
	}
	if got := simple["Body"].(map[string]any)["Text"].(map[string]any)["Data"]; got != "in #general and 2 DMs" {
		t.Errorf("body is %v", got)
	}
}

// SES puts the reason in the body — a sandbox account refusing an unverified
// recipient is a 400 that looks like any other until you read it. Losing that
// text is losing the only diagnosis the operator gets.
func TestSESSurfacesTheErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"message":"Email address is not verified."}`))
	}))
	defer srv.Close()

	m := &sesMailer{
		keyID: "AKIAEXAMPLE", secret: "secret", region: "eu-west-1",
		from: "gliph@example.com", endpoint: srv.URL, client: srv.Client(),
	}
	err := m.Send("her@example.com", "s", "b")
	if err == nil {
		t.Fatal("a 400 must be an error")
	}
	if !strings.Contains(err.Error(), "not verified") {
		t.Errorf("the SES explanation was dropped: %v", err)
	}
}

// The signature must change when the request changes; a derivation that
// accidentally ignored the payload or the date would still look well-formed.
func TestSignatureCoversPayloadAndDate(t *testing.T) {
	m := &sesMailer{
		keyID: "AKIAEXAMPLE", secret: "secret", region: "eu-west-1",
		from: "gliph@example.com", endpoint: "https://email.eu-west-1.amazonaws.com/v2/email/outbound-emails",
	}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	sign := func(payload string, when time.Time) string {
		req, _ := http.NewRequest("POST", m.endpoint, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		m.sign(req, []byte(payload), when)
		return req.Header.Get("Authorization")
	}

	base := sign(`{"a":1}`, at)
	if base == sign(`{"a":2}`, at) {
		t.Error("a different payload produced the same signature")
	}
	if base == sign(`{"a":1}`, at.Add(time.Hour)) {
		t.Error("a different timestamp produced the same signature")
	}
	if base != sign(`{"a":1}`, at) {
		t.Error("signing is not deterministic")
	}
}
