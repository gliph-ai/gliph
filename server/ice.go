package main

// ICE configuration for peer-to-peer transfers (spec F7, "TURN relay fallback").
//
// WHY THE SERVER IS INVOLVED AT ALL. It isn't, in the transfer — the bytes never
// come near it, which is the whole claim F7 makes. But a browser cannot reach a
// peer behind a symmetric NAT without a relay, and roughly a fifth to a third of
// real-world pairs are. Without a TURN server those transfers do not fall back;
// they simply fail. Somebody has to tell the client where the relay is and hand
// it a credential, and the server is the only party already authenticated to
// both peers.
//
// EPHEMERAL CREDENTIALS, NOT A SHARED PASSWORD. coturn's `use-auth-secret` mode
// takes `username = <unix-expiry>:<anything>` and
// `credential = base64(HMAC-SHA1(secret, username))`. The static secret stays on
// the server and on the TURN box; what reaches a client is a credential that
// stops working in a few hours. A long-lived TURN password handed to every
// browser is a relay anyone who reads one can use forever.
//
// THE USERNAME IS RANDOM ON PURPOSE. coturn only needs it to be unique-ish, and
// it lands in the relay's logs. Putting the nick there would hand whoever runs
// the TURN server a timestamped record of which named users were transferring
// files — a social graph, in plaintext, from the one part of gliph that
// otherwise reveals nothing. A random handle costs nothing and says nothing.
//
// A server configured with neither STUN nor TURN returns an empty list. The
// client then falls back to its own default and direct transfers still work;
// only the relayed ones do not, which is exactly the situation before this file.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Long enough that a big transfer cannot outlive its credential, short enough
// that a leaked one is not a lasting gift.
const iceCredentialTTL = 12 * time.Hour

// ICEConfig is what an operator sets. All of it is optional.
type ICEConfig struct {
	// Comma-separated, e.g. "stun:stun.example.com:3478".
	STUN string
	// Comma-separated, e.g. "turn:turn.example.com:3478?transport=udp".
	TURN string
	// coturn's static-auth-secret. Without it, TURN URLs are served with no
	// credential, which every sane relay will refuse — so treat it as required
	// whenever TURN is set.
	Secret string
}

type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// turnCredential mints one ephemeral coturn credential.
func turnCredential(secret string) (user, cred string) {
	buf := make([]byte, 8)
	rand.Read(buf)
	user = fmt.Sprintf("%d:%s", time.Now().Add(iceCredentialTTL).Unix(), hex.EncodeToString(buf))
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(user))
	return user, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// iceServers builds the list for one client.
func (c ICEConfig) iceServers() []iceServer {
	out := []iceServer{}
	if urls := splitList(c.STUN); len(urls) > 0 {
		out = append(out, iceServer{URLs: urls})
	}
	if urls := splitList(c.TURN); len(urls) > 0 && c.Secret != "" {
		user, cred := turnCredential(c.Secret)
		out = append(out, iceServer{URLs: urls, Username: user, Credential: cred})
	}
	return out
}

func (s *Server) iceRoutes(mux *http.ServeMux) {
	// Authenticated: a credential for someone else's relay bandwidth is not
	// something to hand out to anyone who asks.
	mux.HandleFunc("GET /api/ice", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.authUser(r); err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		// Never cached: the credential inside expires, and a cached copy would
		// be served past its own death.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"ice_servers": s.ICE.iceServers()})
	})
}
