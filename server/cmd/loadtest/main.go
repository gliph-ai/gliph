// Command loadtest drives a running Gliph server with N real WebSocket clients
// spread across M channels and reports message delivery latency.
//
// It needs a FRESH server: the first account it registers becomes admin and
// mints the invites for the rest. Run the server pinned the way the spec asks
// ("500 concurrent clients on a 2-vCPU box"):
//
//	cd server && go build -o /tmp/gliph . && GOMAXPROCS=2 /tmp/gliph \
//	    -data "$(mktemp -d)" -addr 127.0.0.1:8899 -plaintext -http-addr ""
//	go run ./cmd/loadtest -url http://127.0.0.1:8899 -clients 500 -channels 50 -duration 10m
//
// Latency is measured in this process: the sender stamps its clock into the
// body and every OTHER member of the channel subtracts it on receipt, so no
// clock sync is involved. The load generator is on the same box as the server;
// keep it off the server's CPUs (taskset/cpuset) if the numbers matter.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	base     = flag.String("url", "http://127.0.0.1:8899", "server base URL")
	clients  = flag.Int("clients", 500, "simulated clients")
	channels = flag.Int("channels", 50, "channels the clients are spread across")
	every    = flag.Duration("every", 5*time.Second, "mean interval between one client's messages")
	duration = flag.Duration("duration", 10*time.Minute, "measured run length")
)

type frame struct {
	T string          `json:"t"`
	D json.RawMessage `json:"d"`
}

func post(path, token string, body any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", *base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %d %v", path, res.StatusCode, out)
	}
	return out, nil
}

func register(nick, invite string) string {
	out, err := post("/api/register", "", map[string]string{
		"nick": nick, "email": nick + "@load.test", "password": "loadtest-password", "invite": invite})
	if err != nil {
		log.Fatal(err)
	}
	return out["token"].(string)
}

// stats collects delivery latencies. ponytail: one mutex and a slice; 10 min at
// ~1k deliveries/s is ~600k floats, which is nothing.
type stats struct {
	mu  sync.Mutex
	lat []float64 // ms
}

func (s *stats) add(ms float64) { s.mu.Lock(); s.lat = append(s.lat, ms); s.mu.Unlock() }

func (s *stats) take() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lat
	s.lat = nil
	return l
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)

	admin := register("loadadmin", "")
	chanIDs := make([]string, *channels)
	chanNames := make([]string, *channels)
	for i := range chanIDs {
		chanNames[i] = fmt.Sprintf("load%02d", i)
		out, err := post("/api/admin/channels", admin, map[string]string{"name": chanNames[i]})
		if err != nil {
			log.Fatal(err)
		}
		chanIDs[i] = out["id"].(string)
	}

	log.Printf("registering %d clients", *clients)
	tokens := make([]string, *clients)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			inv, err := post("/api/admin/invites", admin, map[string]string{})
			if err != nil {
				log.Fatal(err)
			}
			tokens[i] = register(fmt.Sprintf("load%03d", i), inv["invite"].(string))
		}()
	}
	wg.Wait()

	var (
		st                  stats
		sent, recv, notices atomic.Int64
		measuring           atomic.Bool
		joined              sync.WaitGroup
		ctx, cancel         = context.WithCancel(context.Background())
		wsURL               = strings.Replace(*base, "http", "ws", 1)
		perChannel          = make([]int, *channels)
	)
	for i := range tokens {
		perChannel[i%*channels]++
	}

	log.Printf("connecting and joining")
	joined.Add(*clients)
	conns := make([]*websocket.Conn, *clients)
	for i, tok := range tokens {
		c, _, err := websocket.Dial(ctx, wsURL+"/ws?token="+tok, nil)
		if err != nil {
			log.Fatalf("dial %d: %v", i, err)
		}
		c.SetReadLimit(8 << 20)
		conns[i] = c
		me := fmt.Sprintf("load%03d", i)
		ch := i % *channels
		go func() {
			once := sync.Once{}
			for {
				_, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				var f frame
				if json.Unmarshal(b, &f) != nil {
					continue
				}
				switch f.T {
				case "state":
					// Joined once the channel shows up in our rail.
					if bytes.Contains(f.D, []byte(chanIDs[ch])) {
						once.Do(joined.Done)
					}
				case "notice":
					notices.Add(1)
				case "event":
					var e struct{ Kind, Actor, Body string }
					json.Unmarshal(f.D, &e)
					if e.Kind != "message" || e.Actor == me || !strings.HasPrefix(e.Body, "lt ") {
						continue
					}
					ns, _ := strconv.ParseInt(e.Body[3:], 10, 64)
					if measuring.Load() {
						st.add(float64(time.Now().UnixNano()-ns) / 1e6)
						recv.Add(1)
					}
				}
			}
		}()
		send(ctx, c, "command.run", map[string]string{"name": "join", "args": "#" + chanNames[ch]})
	}
	joined.Wait()
	log.Printf("all %d joined; sending one message per client every ~%v for %v", *clients, *every, *duration)

	measuring.Store(true)
	for i, c := range conns {
		ch := chanIDs[i%*channels]
		go func() {
			time.Sleep(rand.N(*every)) // spread the first sends
			t := time.NewTicker(*every)
			defer t.Stop()
			for {
				body := "lt " + strconv.FormatInt(time.Now().UnixNano(), 10)
				if send(ctx, c, "message.send", map[string]string{"channel": ch, "body": body, "nonce": body}) == nil {
					sent.Add(1)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	var all []float64
	report := func(label string, l []float64) {
		slices.Sort(l)
		log.Printf("%s deliveries=%d p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms",
			label, len(l), pct(l, .5), pct(l, .95), pct(l, .99), pct(l, 1))
	}
	end := time.After(*duration)
	tick := time.NewTicker(time.Minute)
loop:
	for {
		select {
		case <-tick.C:
			l := st.take()
			all = append(all, l...)
			report("minute", l)
		case <-end:
			break loop
		}
	}
	time.Sleep(2 * time.Second) // let in-flight messages land
	measuring.Store(false)
	cancel()
	all = append(all, st.take()...)

	// Each message should reach every other member of its channel.
	expected := int64(0)
	if n := sent.Load(); n > 0 {
		expected = n * int64(perChannel[0]-1)
	}
	report("TOTAL ", all)
	log.Printf("sent=%d delivered=%d expected~%d notices=%d", sent.Load(), recv.Load(), expected, notices.Load())
}

func send(ctx context.Context, c *websocket.Conn, t string, d any) error {
	b, _ := json.Marshal(map[string]any{"t": t, "d": d})
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}
