package main

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A fixed-window counter per key: enough to stop a script from turning signup
// into free bcrypt work or the resend button into a mail cannon.
//
// ponytail: in memory, one process. The control plane runs one replica; move
// the counters into PostgreSQL when it runs more.
type limiter struct {
	mu sync.Mutex
	w  map[string]*window
}

type window struct {
	n     int
	reset time.Time
}

func newLimiter() *limiter { return &limiter{w: map[string]*window{}} }

// allow counts one hit on key and says whether it is within n per per.
func (l *limiter) allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w := l.w[key]
	if w == nil || now.After(w.reset) {
		if len(l.w) > 100_000 {
			l.w = map[string]*window{} // a flood of keys: start over rather than grow
		}
		w = &window{reset: now.Add(per)}
		l.w[key] = w
	}
	w.n++
	return w.n <= n
}

// trustedProxies are the peers whose X-Forwarded-For is believed (-trusted-proxy):
// loopback by default, the proxy's network when configured. Same rule as
// server/ratelimit.go.
var trustedProxies = mustCIDRs("127.0.0.0/8,::1/128")

func mustCIDRs(list string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range strings.Split(list, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			log.Fatalf("-trusted-proxy %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

// clientIP is the address a limit is counted against: the last
// X-Forwarded-For entry when (and only when) the peer is a trusted proxy,
// since that entry is the one the proxy appended. Anyone else's header is
// ignored: it would let a caller rotate it past every limit.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, n := range trustedProxies {
			if n.Contains(ip) {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					parts := strings.Split(xff, ",")
					return strings.TrimSpace(parts[len(parts)-1])
				}
				break
			}
		}
	}
	return host
}
