package main

import (
	"sync"
	"testing"
	"time"
)

// The read pool exists so one slow read cannot stall the server. Both halves
// matter: reads must actually run in parallel, and the pool must refuse writes
// so a misrouted statement fails here rather than intermittently under load.
func TestReadPoolIsConcurrentAndReadOnly(t *testing.T) {
	st, err := OpenStore(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ch, err := st.CreateChannel("bench", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := st.Append(&Event{Channel: ch.ID, Kind: "msg", Actor: "w", Body: "hello"}, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Eight readers, each holding a statement open for ~50ms. Serialized on one
	// connection that is 400ms; in parallel it is ~50ms. The 250ms bar fails on a
	// single shared connection and passes on a pool, without being flaky on a
	// loaded CI box.
	const readers = 8
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := st.r.Query(`SELECT id,seq,body FROM events WHERE channel_id=? ORDER BY seq`, ch.ID)
			if err != nil {
				t.Error(err)
				return
			}
			defer rows.Close()
			rows.Next()
			time.Sleep(50 * time.Millisecond) // hold the connection, as a slow scan would
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("%d concurrent reads took %s — the read pool is serialized (readPool=%d?)",
			readers, elapsed.Round(time.Millisecond), readPool)
	}

	// A write on the read pool must be refused. Append's deferred-transaction
	// upgrade is the reason: routed here it would surface as a rare
	// SQLITE_BUSY_SNAPSHOT under load instead of an error anyone can see.
	if _, err := st.r.Exec(`UPDATE channels SET topic='nope' WHERE id=?`, ch.ID); err == nil {
		t.Fatal("read pool accepted a write; query_only is not in effect")
	}

	// ...and the writer still works, so query_only did not leak onto it.
	if err := st.SetTopic(ch.ID, "fine"); err != nil {
		t.Fatalf("writer refused a write: %v", err)
	}
}
