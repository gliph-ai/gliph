package main

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
)

// Prometheus text exposition, hand-written. The format is three lines of
// fmt.Fprintf per series; client_golang would drag in a handful of transitive
// dependencies to produce the same bytes.
//
// PRIVACY — read before adding a series. gliph is end-to-end encrypted and
// /metrics is the endpoint an operator is most likely to expose to a scraper,
// a dashboard, or a hosted monitoring vendor. Nothing here carries a label:
// no nick, no channel name or id, no message body, no file name, no IP. Counts
// and byte totals only. A per-channel or per-user metric would leak the social
// graph in plaintext from an otherwise encrypted product — do not add one.
var (
	mEvents  atomic.Int64 // events appended since start
	mDropped atomic.Int64 // frames dropped by the rate limiter
)

func (s *Server) writeMetrics(w io.Writer) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	s.mu.RLock()
	conns := 0
	for _, list := range s.conns {
		conns += len(list)
	}
	plugins := len(s.plugins)
	s.mu.RUnlock()

	m := func(kind, name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, v)
	}
	gauge := func(name, help string, v any) { m("gauge", name, help, v) }
	counter := func(name, help string, v any) { m("counter", name, help, v) }

	gauge("gliph_connections", "Open client websocket connections.", conns)
	gauge("gliph_plugins_connected", "Connected plugin/agent websockets.", plugins)
	gauge("gliph_users_total", "Registered accounts.", s.St.CountUsers())
	gauge("gliph_channels_total", "Channels including DMs.", s.St.metricCount(`SELECT COUNT(*) FROM channels`))
	counter("gliph_events_total", "Events appended to the log since server start.", mEvents.Load())
	counter("gliph_messages_dropped_ratelimit_total", "Frames dropped by the rate limiter.", mDropped.Load())
	gauge("gliph_uploads_bytes", "Bytes received across stored uploads.",
		s.St.metricCount(`SELECT COALESCE(SUM(received),0) FROM uploads`))
	gauge("gliph_db_bytes", "SQLite database size on disk.",
		s.St.dbBytes())

	gauge("go_goroutines", "Goroutines currently running.", runtime.NumGoroutine())
	gauge("go_memstats_alloc_bytes", "Heap bytes allocated and in use.", ms.Alloc)
	gauge("go_memstats_sys_bytes", "Bytes obtained from the OS.", ms.Sys)
	counter("go_memstats_gc_cycles_total", "Completed GC cycles.", ms.NumGC)
}

// metricCount runs a scalar query, returning 0 rather than an error: a metrics
// scrape must never fail because one number was unavailable.
// These are the whole-table counts — a Prometheus scrape and an admin console
// refresh both land here. On the writer connection they would stall live
// messages behind them, so they read from the pool.
func (s *Store) metricCount(q string) int64 {
	var n int64
	s.r.QueryRow(q).Scan(&n)
	return n
}
