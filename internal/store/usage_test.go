package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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

func runRec(id, run string, cost float64) usage.Record {
	r := rec(id, "/dev/copresence", cost)
	r.RunID = run
	return r
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

func TestRunFilterMatchesOnAPrefix(t *testing.T) {
	// Run ids are UUIDs. Nobody types one, so the filter has to work the way
	// git's short hashes do or the feature goes unused.
	ctx := context.Background()
	st := openU(t)
	recs := []usage.Record{
		runRec("1", "4afcba68-e360-4fd9-9ebe-b3529ab0d771", 10),
		runRec("2", "4afcba68-e360-4fd9-9ebe-b3529ab0d771", 5),
		runRec("3", "97dfea59-8a4b-4be7-9a31-d9e008e35367", 7),
	}
	if _, err := st.RecordUsage(ctx, "main", recs); err != nil {
		t.Fatal(err)
	}
	got, err := st.UsageTotal(ctx, "main", Filter{RunPrefix: "4afcba68"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Calls != 2 || got.CostUSD != 15 {
		t.Errorf("run prefix = %d calls / $%.2f, want 2 / $15.00", got.Calls, got.CostUSD)
	}
	all, err := st.UsageBy(ctx, "main", "run", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("by run = %d groups, want 2", len(all))
	}
	// Qualified by source so two runtimes with overlapping id schemes cannot
	// silently merge into one row once a Codex importer exists.
	if all[0].Key != "test:4afcba68-e360-4fd9-9ebe-b3529ab0d771" {
		t.Errorf("run key = %q, want it qualified by source", all[0].Key)
	}
}

func TestUnattributedSpendJoinsNoRun(t *testing.T) {
	// A row with no run recorded belongs to no run. Letting it fall into
	// whichever run was asked about would inflate that run with work it never
	// did — and the number would look perfectly reasonable.
	ctx := context.Background()
	st := openU(t)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{
		runRec("1", "4afcba68", 10),
		runRec("2", "", 99),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UsageTotal(ctx, "main", Filter{RunPrefix: "4afcba68"})
	if got.CostUSD != 10 {
		t.Errorf("filtered total = $%.2f, want $10 — the unattributed row must not join", got.CostUSD)
	}
	// It stays visible unfiltered, under its own heading, rather than vanishing.
	rows, _ := st.UsageBy(ctx, "main", "run", Filter{})
	var unknown bool
	for _, r := range rows {
		if r.Key == "(unknown)" {
			unknown = true
		}
	}
	if !unknown {
		t.Errorf("unattributed spend disappeared from the run breakdown: %+v", rows)
	}
}

func TestReimportBackfillsRunIDWithoutDisturbingCWD(t *testing.T) {
	// The two byproduct columns arrived in different versions, so a database
	// can be missing either one. Backfilling one must not clobber the other.
	ctx := context.Background()
	st := openU(t)
	first := rec("1", "/dev/copresence", 3)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{first}); err != nil {
		t.Fatal(err)
	}
	second := runRec("1", "4afcba68", 999)
	second.CWD = "/somewhere/else"
	w, err := st.RecordUsage(ctx, "main", []usage.Record{second})
	if err != nil {
		t.Fatal(err)
	}
	if w.Inserted != 0 || w.Backfilled != 1 {
		t.Errorf("write = %+v, want 0 inserted and 1 backfilled", w)
	}
	byRun, _ := st.UsageBy(ctx, "main", "run", Filter{})
	if len(byRun) != 1 || byRun[0].Key != "test:4afcba68" {
		t.Fatalf("run id was not backfilled: %+v", byRun)
	}
	byProject, _ := st.UsageBy(ctx, "main", "project", Filter{})
	if len(byProject) != 1 || byProject[0].Key != "/dev/copresence" {
		t.Errorf("a populated cwd was overwritten: %+v", byProject)
	}
	if byRun[0].CostUSD != 3 {
		t.Errorf("cost = $%.2f, want the original $3", byRun[0].CostUSD)
	}
}

func TestNothingToBackfillIsNotCountedAsWork(t *testing.T) {
	// Re-importing an unchanged transcript must report zero, not "N backfilled".
	// This is the same lie the Inserted/Backfilled split was introduced to fix.
	ctx := context.Background()
	st := openU(t)
	r := runRec("1", "4afcba68", 3)
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{r}); err != nil {
		t.Fatal(err)
	}
	w, err := st.RecordUsage(ctx, "main", []usage.Record{r})
	if err != nil {
		t.Fatal(err)
	}
	if w.Inserted != 0 || w.Backfilled != 0 {
		t.Errorf("re-import of an unchanged row = %+v, want all zeroes", w)
	}
}

func TestSummarySpansTheGroupInTime(t *testing.T) {
	// The clock span is how a human recognizes which run was which; a wrong one
	// mislabels the row rather than merely looking odd.
	ctx := context.Background()
	st := openU(t)
	var recs []usage.Record
	for i, h := range []int{9, 14, 11} {
		r := runRec(fmt.Sprint(i), "4afcba68", 1)
		r.TS = time.Date(2026, 7, 25, h, 0, 0, 0, time.UTC)
		recs = append(recs, r)
	}
	if _, err := st.RecordUsage(ctx, "main", recs); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UsageBy(ctx, "main", "run", Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if h := got[0].First.UTC().Hour(); h != 9 {
		t.Errorf("first = %02d:00, want 09:00", h)
	}
	if h := got[0].Last.UTC().Hour(); h != 14 {
		t.Errorf("last = %02d:00, want 14:00", h)
	}
}
