package main

// Direct FCM (spec F5), the Android twin of apns.go. The server talks to
// Google itself with the Firebase project's service account, so Android push
// needs no relay.
//
// The same nothing crosses the wire: a data-only message whose one field is
// `wake: "1"`. There is no `notification` block, so Android never draws one
// on its own; the app wakes, re-syncs, decrypts and writes the notification
// locally. Google learns that a device was woken and when, nothing else.
//
// ponytail: no Firebase Admin SDK. Auth is one RS256 JWT traded for an OAuth
// token, delivery is one POST per device. A dependency tree that size would
// need auditing for exactly this privacy contract.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

type fcmSender struct {
	key      *rsa.PrivateKey
	email    string
	tokenURI string // OAuth exchange, from the service-account JSON
	sendURL  string // .../v1/projects/<id>/messages:send
	client   *http.Client
	// forget drops a token Google has rejected, offered to every team the
	// process serves (see apns.go).
	forget func(token string)

	mu       sync.Mutex
	access   string
	accessTo time.Time
}

// NewFCMSender reads the service-account JSON downloaded from the Firebase
// console (Project settings → Service accounts → Generate new private key).
func NewFCMSender(forget func(token string), credPath string) (*fcmSender, error) {
	raw, err := os.ReadFile(credPath)
	if err != nil {
		return nil, err
	}
	var sa struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("%s: %w", credPath, err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("%s: not a service-account key (needs project_id, client_email, private_key)", credPath)
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("%s: private_key is not PEM", credPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", credPath, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: private_key is not RSA", credPath)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &fcmSender{
		key: rk, email: sa.ClientEmail, tokenURI: sa.TokenURI, forget: forget,
		sendURL: "https://fcm.googleapis.com/v1/projects/" + sa.ProjectID + "/messages:send",
		client:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Wake is fire and forget, like APNs: a missed wake is a phone that catches
// up on its next foreground.
func (f *fcmSender) Wake(devices []PushDevice) { go f.wakeAll(devices) }

func (f *fcmSender) wakeAll(devices []PushDevice) {
	for _, d := range devices {
		if d.Platform == "android" {
			f.send(d.Token)
		}
	}
}

func (f *fcmSender) send(token string) {
	bearer, err := f.bearer()
	if err != nil {
		log.Printf("fcm: %v", err)
		return
	}
	// The whole message. `data` holds one flag and there is no notification
	// block, on purpose: see the top of this file.
	msg, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token":   token,
		"data":    map[string]string{"wake": "1"},
		"android": map[string]string{"priority": "high"},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", f.sendURL, bytes.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := f.client.Do(req)
	if err != nil {
		log.Printf("fcm: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return
	}
	var body struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&body)
	switch {
	case resp.StatusCode == 404 || body.Error.Status == "UNREGISTERED" || body.Error.Status == "INVALID_ARGUMENT":
		// The app was uninstalled, or the token is not one FCM will ever take.
		if f.forget != nil {
			f.forget(token)
		}
	case resp.StatusCode == 401:
		f.mu.Lock()
		f.access = "" // mint a fresh one next time
		f.mu.Unlock()
		log.Printf("fcm: HTTP 401 %s", body.Error.Message)
	default:
		log.Printf("fcm: HTTP %d %s", resp.StatusCode, body.Error.Status)
	}
}

// bearer is the OAuth access token, minted from a signed assertion and reused
// until five minutes before Google says it expires.
func (f *fcmSender) bearer() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access != "" && time.Now().Before(f.accessTo) {
		return f.access, nil
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now().Unix()
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": f.email, "scope": fcmScope, "aud": f.tokenURI, "iat": now, "exp": now + 3600})
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + base64.RawURLEncoding.EncodeToString(sig)},
	}
	resp, err := f.client.Post(f.tokenURI, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&tok)
	if resp.StatusCode != 200 || tok.AccessToken == "" {
		return "", fmt.Errorf("token exchange: HTTP %d", resp.StatusCode)
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	f.access = tok.AccessToken
	f.accessTo = time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second - 5*time.Minute)
	return f.access, nil
}
