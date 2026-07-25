package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"path/filepath"

	"github.com/lll-lll-lll-lll/copresence/internal/assemble"
	"github.com/lll-lll-lll-lll/copresence/internal/event"
	"github.com/lll-lll-lll-lll/copresence/internal/store"
)

// openStore is the package-external twin of the in-package helper; these tests
// live outside package store because they exercise assemble on top of it.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), store.DirName, "session.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The daemonless design makes exactly one bet: that several processes reading
// and writing the same SQLite file stay consistent without a coordinator. These
// tests are that bet. They live in the store package rather than in assemble
// because the fake Source cannot express a head that moves under the reader.

func TestCatchupDeliversEveryEventUnderConcurrentWrites(t *testing.T) {
	// Regression: Build used to read Unread and MaxSeq as independent
	// statements, then advance the watermark to MaxSeq. Anything written in
	// between was marked read without ever being delivered — silently, with no
	// error and no fold notice. Measured at ~9% loss at 300 events.
	const total = 400
	ctx := context.Background()
	st := openStore(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= total; i++ {
			if _, err := st.Post(ctx, &event.Event{
				Session: "main", Actor: "writer", Type: event.Note,
				Body: fmt.Sprintf("MARK-%d", i),
			}); err != nil {
				t.Errorf("post %d: %v", i, err)
				return
			}
		}
	}()

	delivered := map[string]bool{}
	deliver := func() {
		res, err := assemble.Build(ctx, st, assemble.Request{
			Session: "main", Participant: "reader", Budget: 1 << 20,
		})
		if err != nil {
			t.Errorf("build: %v", err)
			return
		}
		for _, e := range mustUnreadWindow(t, st, res.AsOf) {
			delivered[e.Body] = true
		}
		if err := st.Advance(ctx, "main", "reader", res.AsOf); err != nil {
			t.Errorf("advance: %v", err)
		}
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
loop:
	for {
		select {
		case <-done:
			break loop
		default:
			deliver()
		}
	}
	deliver() // drain whatever landed after the writer finished

	var missing []int
	for i := 1; i <= total; i++ {
		if !delivered[fmt.Sprintf("MARK-%d", i)] {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		show := missing
		if len(show) > 10 {
			show = show[:10]
		}
		t.Fatalf("%d of %d events were never delivered but were marked read: %v...",
			len(missing), total, show)
	}
}

// mustUnreadWindow re-reads what the reader was entitled to up to asOf. The
// projection itself is budget-limited, so asserting on its rendered text would
// conflate "not delivered" with "folded"; the watermark is the thing under test.
func mustUnreadWindow(t *testing.T, st *store.Store, asOf int64) []event.Event {
	t.Helper()
	evs, err := st.Unread(context.Background(), "main", "reader", asOf, 1<<30)
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	return evs
}

func TestBacklogLargerThanOnePageIsResumedNotSkipped(t *testing.T) {
	// Regression: Unread caps at 500 rows but AsOf came from MaxSeq, so on a
	// 620-event backlog the last 120 became permanently unreachable.
	const total = store.DefaultUnreadLimit + 120
	ctx := context.Background()
	st := openStore(t)
	for i := 1; i <= total; i++ {
		if _, err := st.Post(ctx, &event.Event{
			Session: "main", Actor: "writer", Type: event.Note, Body: fmt.Sprintf("MARK-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	for round := 0; round < 5; round++ {
		res, err := assemble.Build(ctx, st, assemble.Request{
			Session: "main", Participant: "reader", Budget: 1 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 && !res.Truncated {
			t.Error("a backlog beyond one page should report itself as truncated")
		}
		for _, e := range mustUnreadWindow(t, st, res.AsOf) {
			seen[e.Body] = true
		}
		if err := st.Advance(ctx, "main", "reader", res.AsOf); err != nil {
			t.Fatal(err)
		}
		if len(seen) == total {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("delivered %d of %d events across repeated catchups", len(seen), total)
	}
}

func TestConcurrentWritersAllLand(t *testing.T) {
	const writers, each = 8, 40
	ctx := context.Background()
	st := openStore(t)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := st.Post(ctx, &event.Event{
					Session: "main", Actor: fmt.Sprintf("w%d", w), Type: event.Note,
					Body: fmt.Sprintf("w%d-%d", w, i),
				}); err != nil {
					t.Errorf("w%d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	got, err := st.Since(ctx, "main", 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers*each {
		t.Errorf("got %d events, want %d", len(got), writers*each)
	}
	seqs := map[int64]bool{}
	for _, e := range got {
		if seqs[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seqs[e.Seq] = true
	}
	// The FTS index is maintained by trigger; a desync here would make folded
	// content unrecoverable exactly when the log is busiest.
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events_fts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != writers*each {
		t.Errorf("fts has %d rows, events has %d", n, writers*each)
	}
}
