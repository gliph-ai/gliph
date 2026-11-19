package main

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Token buckets, keyed per *user* rather than per connection: someone with a
// phone, a laptop and a tab open gets one budget, not three.
//
// The numbers below are picked so a human never meets them and a script always
// does. A fast typist peaks around 1 message/second; 5/s sustained with a burst
// of 20 leaves an order of magnitude of headroom, while the spec's "100 messages
// in 5 seconds" attacker gets ~20 through and is then held to 5/s with a visible
// notice on every dropped frame.
//
//	messages  burst 20, 5/s      — chat is bursty; paste-storms are not
//	commands  burst 10, 1/s      — each one can hit the DB or a plugin
//	room      burst 80, 40/s     — 10 Hz movement + 10 Hz object streams + ICE bursts
//	uploads   burst  5, 1/10s    — one create per file, resume PATCHes are free
//	login/nick burst 5, 1/30s    — 5 tries then one guess per 30s per account
//	login/IP  burst 20, 1/5s     — a shared office NAT stays usable, a botnet does not
//
// Exceeding a budget never disconnects and never fails silently: the frame is
// dropped and the user is told, so the client can show it.
const (
	maxConnsPerUser = 8 // stops one account exhausting sockets
	limiterIdle     = 15 * time.Minute
)

var errTooFast = errors.New("you are sending too fast — wait a moment")

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a token bucket per key. Safe for concurrent use.
type Limiter struct {
	rate  float64 // tokens refilled per second
	burst float64 // bucket size
	mu    sync.Mutex
	b     map[string]*bucket
}

func NewLimiter(rate, burst float64) *Limiter {
	return &Limiter{rate: rate, burst: burst, b: map[string]*bucket{}}
}

func (l *Limiter) Allow(key string) bool { return l.allowAt(key, time.Now()) }

func (l *Limiter) allowAt(key string, t time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	bk := l.b[key]
	if bk == nil {
		bk = &bucket{tokens: l.burst, last: t}
		l.b[key] = bk
	}
	if d := t.Sub(bk.last).Seconds(); d > 0 {
		bk.tokens = min(bk.tokens+d*l.rate, l.burst)
		bk.last = t
	}
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// pruneAt drops buckets nobody has touched for `idle`. Dropping one resets it to
// full, which is only correct while idle > burst/rate — true for every budget
// configured below, and asserted by TestLimiterPruneNeverGrantsBudget.
func (l *Limiter) pruneAt(t time.Time, idle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, bk := range l.b {
		if t.Sub(bk.last) > idle {
			delete(l.b, k)
		}
	}
}

func (l *Limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.b)
}

// Limits is the whole server's flood protection in one place.
type Limits struct {
	Messages  *Limiter
	Commands  *Limiter
	Rooms     *Limiter
	Uploads   *Limiter
	LoginNick *Limiter
	LoginIP   *Limiter
	MaxConns  int
}

func NewLimits() *Limits {
	return &Limits{
		Messages:  NewLimiter(5, 20),
		Commands:  NewLimiter(1, 10),
		Rooms:     NewLimiter(40, 80),
		Uploads:   NewLimiter(0.1, 5),
		LoginNick: NewLimiter(1.0/30, 5),
		LoginIP:   NewLimiter(0.2, 20),
		MaxConns:  maxConnsPerUser,
	}
}

func (l *Limits) all() []*Limiter {
	return []*Limiter{l.Messages, l.Commands, l.Rooms, l.Uploads, l.LoginNick, l.LoginIP}
}

// Prune keeps the bucket maps from growing with every nick and IP ever seen.
func (l *Limits) Prune() {
	now := time.Now()
	for _, x := range l.all() {
		x.pruneAt(now, limiterIdle)
	}
}

// throttled reports whether this frame must be dropped, and if so tells the user
// why in the channel they were typing in.
func (s *Server) throttled(c *Conn, kind, cid string) bool {
	var l *Limiter
	switch kind {
	case "message":
		l = s.Limits.Messages
	case "command":
		l = s.Limits.Commands
	case "room":
		l = s.Limits.Rooms
	default:
		return false
	}
	if l.Allow(c.user.ID) {
		return false
	}
	mDropped.Add(1)
	s.errTo(c, cid, errTooFast)
	return true
}

// trustedProxies are the peers whose X-Forwarded-For is believed (-trusted-proxy).
// Loopback by default, the usual local reverse proxy; on Pushleft the router
// sits in the environment's own network (10.128.0.0/9), which only this
// project's containers share.
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

// clientIP is the address login attempts are counted against. Behind a reverse
// proxy every request reports the proxy's address, which would collapse all
// logins into one bucket and lock the whole server out. So when (and only when)
// the peer is a trusted proxy, believe the last X-Forwarded-For entry, the one
// that proxy appended. Anyone else's header is ignored: it is attacker-controlled.
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

// loginAllowed blunts password guessing from both directions: per account (so a
// distributed attack cannot spread the cost) and per source address (so one host
// cannot spray many accounts). Both buckets are always charged.
func (s *Server) loginAllowed(r *http.Request, nick string) bool {
	ok := s.Limits.LoginIP.Allow(clientIP(r))
	if nick = strings.ToLower(strings.TrimSpace(nick)); nick != "" {
		ok = s.Limits.LoginNick.Allow(nick) && ok
	}
	return ok
}
