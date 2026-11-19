package main

// The hosted push relay (spec F14): `gliph -relay`, the other end of a
// self-hosted server's `-push-relay <url>`.
//
// A self-hoster has no APNs key for the gliph app and no FCM service account
// for its Firebase project; only whoever publishes the apps does. The relay
// holds both and forwards wakes: POST {"wake":[{"platform","token"}]} in, one
// content-free push per device out, through the same senders a server with its
// own keys uses (apns.go, fcm.go). The request has no field for anything else,
// so there is nothing else to forward or log (see push.go's honesty note).
//
// Abuse limits. The relay is open: a self-hosted server needs no account to
// use it. What one sender can do is bounded instead:
//
//	per server address   burst 300 wakes, then 2/s   (a /64 for IPv6)
//	per device token     burst 3, then one per 20s   (servers coalesce to one per 30s)
//	per request          100 wakes, 64 KiB
//
// A server over its budget gets 429 for the wakes that did not fit; the rest
// still go. ponytail: limits are in memory, per relay process; one process
// per relay, or a shared store when there are several.

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"
)

const (
	relayMaxWakes = 100
	relayMaxBody  = 64 << 10
)

type pushRelay struct {
	senders  byPlatform
	perIP    *Limiter
	perToken *Limiter
}

func newPushRelay(senders byPlatform) *pushRelay {
	return &pushRelay{senders: senders, perIP: NewLimiter(2, 300), perToken: NewLimiter(1.0/20, 3)}
}

// relayKey is the sending server: its address, or its /64 on IPv6, where one
// host owns the whole prefix.
func relayKey(r *http.Request) string {
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return clientIP(r)
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String()
	}
	return ip.String()
}

func (p *pushRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	case r.Method != http.MethodPost:
		fail(w, 405, "POST wakes here")
		return
	}
	var d struct {
		Wake []wake `json:"wake"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, relayMaxBody))
	dec.DisallowUnknownFields() // a body or a nick is not a wake
	if err := dec.Decode(&d); err != nil {
		fail(w, 400, "body must be {\"wake\":[{\"platform\",\"token\"}]}")
		return
	}
	if len(d.Wake) == 0 || len(d.Wake) > relayMaxWakes {
		fail(w, 400, "1 to 100 wakes per request")
		return
	}
	key := relayKey(r)
	var out []PushDevice
	limited := 0
	for _, wk := range d.Wake {
		if !pushPlatforms[wk.Platform] || wk.Token == "" || len(wk.Token) > 512 {
			continue
		}
		if !p.perIP.Allow(key) {
			limited++
			continue
		}
		if !p.perToken.Allow(wk.Platform + ":" + wk.Token) {
			continue // woken a moment ago; another wake changes nothing
		}
		out = append(out, PushDevice{Platform: wk.Platform, Token: wk.Token})
	}
	if len(out) > 0 {
		p.senders.Wake(out)
	}
	if limited > 0 {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, 429, map[string]int{"sent": len(out), "limited": limited})
		return
	}
	writeJSON(w, 200, map[string]int{"sent": len(out)})
}

func (p *pushRelay) prune() {
	for range time.Tick(10 * time.Minute) {
		p.perIP.pruneAt(time.Now(), limiterIdle)
		p.perToken.pruneAt(time.Now(), limiterIdle)
	}
}

// runRelay is the whole process in relay mode: no store, no teams.
func runRelay(senders byPlatform, lo listenOpts) []*http.Server {
	p := newPushRelay(senders)
	go p.prune()
	log.Printf("push relay (%s)", version)
	return listen(p, lo)
}
