package main

// Direct APNs (spec F5). The server talks to Apple itself, with the team's
// APNs auth key, so mobile push on iOS needs no relay and no third party.
//
// What crosses the wire is the same nothing push.go promises: a background
// push whose only content is `content-available: 1`. Apple learns that a
// device was woken and when; it never sees a body, a nick or a channel.
//
// ponytail: no APNs library. Token auth is one ES256 JWT, delivery is one
// HTTP/2 POST, and net/http speaks h2 to Apple on its own. ~120 lines beats a
// dependency that would need auditing for exactly this privacy contract.
//
// Android is the other half, in fcm.go: the same shape (a service-account
// JWT, one POST per token) and the same empty payload.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// A dev build installed from Xcode registers with the sandbox gateway, a
// TestFlight or App Store build with production, and the token says nothing
// about which. Production is tried first; the sandbox on BadDeviceToken. One
// server, both kinds of phone, no flag to get wrong.
var apnsHosts = []string{"https://api.push.apple.com", "https://api.sandbox.push.apple.com"}

type apnsSender struct {
	key           *ecdsa.PrivateKey
	keyID, teamID string
	topic         string // the app's bundle id
	hosts         []string
	client        *http.Client
	// forget drops a token Apple has rejected. A callback rather than a *Store
	// because one sender serves every team on the process: the token lives in
	// exactly one team's database, and the shard knows which.
	forget func(token string)

	mu    sync.Mutex
	jwt   string
	jwtAt time.Time
}

// NewAPNSSender reads the .p8 auth key from the developer portal. keyID is the
// ten-character id shown next to it, teamID the team's, topic the bundle id.
func NewAPNSSender(forget func(token string), keyPath, keyID, teamID, topic string) (*apnsSender, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file", keyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an EC key", keyPath)
	}
	if keyID == "" || teamID == "" || topic == "" {
		return nil, fmt.Errorf("apns needs -apns-key-id, -apns-team-id and -apns-topic")
	}
	return &apnsSender{key: ec, keyID: keyID, teamID: teamID, topic: topic,
		hosts: apnsHosts, forget: forget, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// Wake is fire and forget, as the relay's is: a push that fails is a phone that
// catches up on its next foreground.
func (a *apnsSender) Wake(devices []PushDevice) { go a.wakeAll(devices) }

func (a *apnsSender) wakeAll(devices []PushDevice) {
	for _, d := range devices {
		if d.Platform == "ios" {
			a.send(d.Token)
		}
	}
}

// The whole payload. There is no field to add a body to, on purpose.
var apnsWake = []byte(`{"aps":{"content-available":1}}`)

func (a *apnsSender) send(token string) {
	for i, host := range a.hosts {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", host+"/3/device/"+token, bytes.NewReader(apnsWake))
		if err != nil {
			cancel()
			return
		}
		req.Header.Set("authorization", "bearer "+a.bearer())
		req.Header.Set("apns-topic", a.topic)
		req.Header.Set("apns-push-type", "background")
		req.Header.Set("apns-priority", "5") // what Apple requires of a background push
		resp, err := a.client.Do(req)
		cancel()
		if err != nil {
			log.Printf("apns: %v", err)
			return
		}
		var body struct{ Reason string }
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == 200:
			return
		case body.Reason == "BadDeviceToken" && i+1 < len(a.hosts):
			continue // the other gateway's phone
		case resp.StatusCode == 410 || body.Reason == "BadDeviceToken":
			// Apple says this token will never work again: an uninstall, or a
			// rotation the phone will report on its next launch.
			if a.forget != nil {
				a.forget(token)
			}
			return
		default:
			log.Printf("apns: HTTP %d %s", resp.StatusCode, body.Reason)
			return
		}
	}
}

// bearer is the provider token: ES256 over {iss: team, iat: now}. Apple wants
// it reused for at least 20 minutes and replaced within the hour.
func (a *apnsSender) bearer() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.jwtAt) < 50*time.Minute {
		return a.jwt
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]string{"alg": "ES256", "kid": a.keyID}) + "." +
		enc(map[string]any{"iss": a.teamID, "iat": time.Now().Unix()})
	sum := sha256.Sum256([]byte(signing))
	// JWS wants r‖s as two fixed 32-byte words, not the ASN.1 that Sign gives.
	r, s, err := ecdsa.Sign(rand.Reader, a.key, sum[:])
	if err != nil {
		return ""
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	a.jwt = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	a.jwtAt = time.Now()
	return a.jwt
}

// DeletePushToken drops a token whichever account holds it: the platform said
// it is dead, and that is not a per-user fact.
func (s *Store) DeletePushToken(tok string) {
	s.db.Exec(`DELETE FROM push_devices WHERE token=?`, tok)
}

// byPlatform hands each platform's devices to its own sender: iOS straight to
// Apple, Android to FCM when that exists, either to the relay when it is the
// only thing configured.
type byPlatform map[string]PushSender

func (b byPlatform) Wake(devices []PushDevice) {
	groups := map[string][]PushDevice{}
	for _, d := range devices {
		groups[d.Platform] = append(groups[d.Platform], d)
	}
	for p, ds := range groups {
		if s := b[p]; s != nil {
			s.Wake(ds)
		}
	}
}
