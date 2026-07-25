package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/store"
	"github.com/lll-lll-lll-lll/copresence/internal/usage"
)

const usageHelp = `copresence usage — token and cost accounting

  usage import [FILE...]   import Claude Code transcripts (auto-discovers by default)
  usage [report]           show spend by actor, model, and scope
  usage records            raw rows as JSON, newest first (dashboard feed)

Flags: --by actor|model|day|scope|project  --since 7d  --json  --limit N
       --all-projects  include work done outside this workspace

Reports cover this workspace only. Transcripts are keyed by the directory a
session started in, so one opened in a parent directory also carries sibling
projects' spend; --all-projects shows it, --by project breaks it down.
`

func cmdUsage(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "import":
			return cmdUsageImport(args[1:])
		case "records":
			return cmdUsageRecords(args[1:])
		case "report":
			args = args[1:]
		case "-h", "--help", "help":
			fmt.Print(usageHelp)
			return nil
		}
	}
	return cmdUsageReport(args)
}

func cmdUsageImport(args []string) error {
	fs := flag.NewFlagSet("usage import", flag.ExitOnError)
	c := bind(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	paths := fs.Args()
	if len(paths) == 0 {
		root, err := findRoot(c.dir)
		if err != nil {
			return err
		}
		var from string
		paths, from, err = usage.DiscoverClaudeCode(root)
		if err != nil {
			return err
		}
		if from != root {
			fmt.Fprintf(os.Stderr, "note: no transcripts for %s; using %s, which may include other projects\n", root, from)
		}
	}

	totalNew, totalSeen, totalFilled := 0, 0, 0
	var unpriced []string
	for _, p := range paths {
		recs, err := usage.ParseClaudeCodeFile(p, c.actor)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", p, err)
			continue
		}
		w, err := st.RecordUsage(ctx, c.session, recs)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if !r.Priced && !contains(unpriced, r.Model) {
				unpriced = append(unpriced, r.Model)
			}
		}
		totalNew += w.Inserted
		totalFilled += w.Backfilled
		totalSeen += len(recs)
		note := ""
		if w.Backfilled > 0 {
			note = fmt.Sprintf(", %d backfilled", w.Backfilled)
		}
		fmt.Printf("%-58s %4d calls, %d new%s\n", trimPath(p), len(recs), w.Inserted, note)
	}
	fmt.Printf("\n%d model calls seen, %d newly recorded", totalSeen, totalNew)
	if totalFilled > 0 {
		fmt.Printf(", %d existing rows backfilled", totalFilled)
	}
	fmt.Println()

	// Unpriced models are reported loudly: a dashboard total that silently
	// omits an unknown model is worse than no total.
	if len(unpriced) > 0 {
		fmt.Fprintf(os.Stderr, "\nwarning: no pricing for %s — those calls count tokens but $0.00\n",
			strings.Join(unpriced, ", "))
	}
	return nil
}

func cmdUsageReport(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	c := bind(fs, false)
	by := fs.String("by", "", "aggregate by actor, model, day, scope, or project")
	sinceFlag := fs.String("since", "", "only usage after this window, e.g. 24h or 7d")
	asJSON := fs.Bool("json", false, "emit JSON")
	all := fs.Bool("all-projects", false, "include work done outside this workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	filter, root, err := c.usageFilter(*sinceFlag, *all)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	dims := []string{"actor", "model", "scope"}
	if *by != "" {
		dims = []string{*by}
	}

	out := map[string]any{}
	total, err := st.UsageTotal(ctx, c.session, filter)
	if err != nil {
		return err
	}
	out["total"] = total
	for _, d := range dims {
		rows, err := st.UsageBy(ctx, c.session, d, filter)
		if err != nil {
			return err
		}
		out[d] = rows
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if total.Calls == 0 {
		fmt.Println("(no usage recorded — run `copresence usage import`)")
		return nil
	}
	fmt.Printf("session %q", c.session)
	if filter.CWDPrefix != "" {
		fmt.Printf(" in %s", trimPath(root))
	} else {
		fmt.Printf(" across all projects")
	}
	if !filter.Since.IsZero() {
		fmt.Printf(" since %s", filter.Since.Local().Format("2006-01-02 15:04"))
	}
	fmt.Printf("\n  %d calls   $%.2f   %s tokens\n",
		total.Calls, total.CostUSD, humanTokens(total.InputTokens+total.OutputTokens+
			total.CacheReadTokens+total.CacheWriteToken))
	if total.UnpricedCalls > 0 {
		fmt.Printf("  (%d calls have no pricing and contribute $0.00)\n", total.UnpricedCalls)
	}
	for _, d := range dims {
		rows := out[d].([]usage.Summary)
		if len(rows) == 0 {
			continue
		}
		fmt.Printf("\nby %s\n", d)
		fmt.Printf("  %-28s %6s %10s %10s %10s %9s\n", "", "calls", "in", "out", "cache", "cost")
		for _, r := range rows {
			fmt.Printf("  %-28s %6d %10s %10s %10s %9s\n", shortKey(r.Key, 28), r.Calls,
				humanTokens(r.InputTokens), humanTokens(r.OutputTokens),
				humanTokens(r.CacheReadTokens+r.CacheWriteToken), fmt.Sprintf("$%.2f", r.CostUSD))
		}
	}
	return nil
}

func cmdUsageRecords(args []string) error {
	fs := flag.NewFlagSet("usage records", flag.ExitOnError)
	c := bind(fs, false)
	sinceFlag := fs.String("since", "", "only usage after this window, e.g. 24h or 7d")
	limit := fs.Int("limit", 1000, "max rows")
	all := fs.Bool("all-projects", false, "include work done outside this workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	filter, _, err := c.usageFilter(*sinceFlag, *all)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	recs, err := st.UsageRecords(context.Background(), c.session, filter, *limit)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(recs)
}

// usageFilter builds the query filter. Reports scope to the workspace by
// default: a transcript directory can span sibling projects, and a total that
// silently includes unrelated work is worse than one that says what it covers.
func (c *common) usageFilter(sinceFlag string, allProjects bool) (store.Filter, string, error) {
	since, err := parseSince(sinceFlag)
	if err != nil {
		return store.Filter{}, "", err
	}
	root, err := findRoot(c.dir)
	if err != nil {
		return store.Filter{}, "", err
	}
	f := store.Filter{Since: since}
	if !allProjects {
		f.CWDPrefix = root
	}
	return f, root, nil
}

// parseSince accepts a Go duration plus a "d" suffix for days, which is what
// people actually type when asking about spend.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return time.Time{}, fmt.Errorf("bad --since %q", s)
		}
		return time.Now().Add(-d * 24), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad --since %q (try 24h or 7d)", s)
	}
	return time.Now().Add(-d), nil
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// shortKey fits a grouping key into n columns. Paths are elided from the left:
// the tail is what distinguishes /Users/me/dev/copresence from
// /Users/me/dev/atcoder, so truncating from the right hides exactly the part
// the reader needs.
func shortKey(s string, n int) string {
	s = trimPath(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if strings.ContainsRune(s, '/') {
		return "…" + string(r[len(r)-(n-1):])
	}
	return string(r[:n-1]) + "…"
}

func trimPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(p, home); ok {
			return "~" + rest
		}
	}
	return p
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
