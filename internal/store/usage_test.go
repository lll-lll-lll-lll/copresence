package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lll-lll-lll-lll/copresence/internal/usage"
)

func openU(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), DirName, "session.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func rec(id, cwd string, cost float64) usage.Record {
	return usage.Record{
		Actor: "a", Source: "test", ExternalID: id, Model: "claude-opus-5",
		CWD: cwd, CostUSD: cost, Priced: true,
	}
}

func TestCWDPrefixScopesOnPathBoundaries(t *testing.T) {
	// A prefix must not match a sibling that merely starts with the same text:
	// /dev/copresence and /dev/copresence-old are different projects.
	ctx := context.Background()
	st := openU(t)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{
		rec("1", "/dev/copresence", 10),
		rec("2", "/dev/copresence/internal", 1),
		rec("3", "/dev/copresence-old", 100),
		rec("4", "/dev/atcoder", 100),
		rec("5", "", 5), // imported before cwd was recorded
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.UsageTotal(ctx, "main", Filter{CWDPrefix: "/dev/copresence"})
	if err != nil {
		t.Fatal(err)
	}
	// 10 + 1 + 5: the exact directory, one below it, and the unknown row.
	if got.CostUSD != 16 {
		t.Errorf("scoped total = $%.2f, want $16 (a sibling prefix or a foreign project leaked in)", got.CostUSD)
	}

	all, err := st.UsageTotal(ctx, "main", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.CostUSD != 216 {
		t.Errorf("unscoped total = $%.2f, want $216", all.CostUSD)
	}
}

func TestUnknownCWDIsReportedNotDropped(t *testing.T) {
	// Rows imported before cwd existed must stay visible. Excluding them would
	// silently shrink historical totals — a report that quietly loses money is
	// worse than one that admits it does not know where it went.
	ctx := context.Background()
	st := openU(t)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{rec("1", "", 7)}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.UsageBy(ctx, "main", "project", Filter{CWDPrefix: "/dev/copresence"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "(unknown)" {
		t.Fatalf("rows = %+v, want one row keyed (unknown)", rows)
	}
}

func TestReimportBackfillsCWDWithoutRestatingCost(t *testing.T) {
	// Re-import is how an older database gains a new column. It must not also
	// reprice: costs are resolved once, at import, so a later rate change cannot
	// rewrite history.
	ctx := context.Background()
	st := openU(t)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{rec("1", "", 3)}); err != nil {
		t.Fatal(err)
	}
	w, err := st.RecordUsage(ctx, "main", []usage.Record{rec("1", "/dev/copresence", 999)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Inserted != 0 || w.Backfilled != 1 {
		t.Errorf("write = %+v, want 0 inserted and 1 backfilled", w)
	}
	got, _ := st.UsageBy(ctx, "main", "project", Filter{})
	if len(got) != 1 || got[0].Key != "/dev/copresence" {
		t.Fatalf("cwd was not backfilled: %+v", got)
	}
	if got[0].CostUSD != 3 {
		t.Errorf("cost = $%.2f, want the original $3 — a re-import must not reprice", got[0].CostUSD)
	}
}
