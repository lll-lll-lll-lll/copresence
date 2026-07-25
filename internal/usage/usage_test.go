package usage

import (
	"math"
	"strings"
	"testing"
	"time"
)

var when = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestCostIncludesEveryTokenClass(t *testing.T) {
	// Hand-computed against a real recorded call:
	//   2 in × $5 + 921 out × $25 + 441411 read × $0.50 + 1943 1h-write × $10
	r := Record{
		Model: "claude-opus-5", TS: when,
		InputTokens: 2, OutputTokens: 921, CacheReadTokens: 441411, CacheWrite1h: 1943,
	}
	r.Cost()
	const want = 0.2631705
	if math.Abs(r.CostUSD-want) > 1e-9 {
		t.Errorf("CostUSD = %.9f, want %.9f", r.CostUSD, want)
	}
	if !r.Priced {
		t.Error("a known model must be priced")
	}
}

func TestCacheWriteTiersArePricedDifferently(t *testing.T) {
	// A 1h write costs 2x input and a 5m write 1.25x. Collapsing the two would
	// understate a cache-heavy workload by 60% on the write line.
	base := func(w5, w1h int64) float64 {
		r := Record{Model: "claude-opus-5", TS: when, CacheWrite5m: w5, CacheWrite1h: w1h}
		r.Cost()
		return r.CostUSD
	}
	fiveMin, oneHour := base(1_000_000, 0), base(0, 1_000_000)
	if math.Abs(fiveMin-6.25) > 1e-9 {
		t.Errorf("5m write of 1M tokens = $%.4f, want $6.25", fiveMin)
	}
	if math.Abs(oneHour-10.0) > 1e-9 {
		t.Errorf("1h write of 1M tokens = $%.4f, want $10.00", oneHour)
	}
}

func TestCacheReadIsATenthOfInput(t *testing.T) {
	r := Record{Model: "claude-opus-5", TS: when, CacheReadTokens: 1_000_000}
	r.Cost()
	if math.Abs(r.CostUSD-0.50) > 1e-9 {
		t.Errorf("1M cache-read tokens = $%.4f, want $0.50", r.CostUSD)
	}
}

func TestFastModeIsPricedSeparately(t *testing.T) {
	// Fast mode is a price, not a model — it cannot be read off the model id.
	std := Record{Model: "claude-opus-5", TS: when, OutputTokens: 1_000_000}
	fast := Record{Model: "claude-opus-5", Speed: "fast", TS: when, OutputTokens: 1_000_000}
	std.Cost()
	fast.Cost()
	if std.CostUSD != 25 || fast.CostUSD != 50 {
		t.Errorf("standard = $%.2f (want 25), fast = $%.2f (want 50)", std.CostUSD, fast.CostUSD)
	}
}

func TestIntroductoryPricingFollowsTheEventTimestamp(t *testing.T) {
	// Prices are applied against when the call happened, not when the import
	// runs, so re-importing an old transcript reproduces the real cost.
	during := Record{Model: "claude-sonnet-5", TS: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), OutputTokens: 1_000_000}
	after := Record{Model: "claude-sonnet-5", TS: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), OutputTokens: 1_000_000}
	during.Cost()
	after.Cost()
	if during.CostUSD != 10 {
		t.Errorf("intro period = $%.2f, want $10", during.CostUSD)
	}
	if after.CostUSD != 15 {
		t.Errorf("after intro = $%.2f, want $15", after.CostUSD)
	}
}

func TestUnknownModelIsFlaggedNotSilentlyFree(t *testing.T) {
	r := Record{Model: "claude-something-unreleased", TS: when, OutputTokens: 1_000_000}
	r.Cost()
	if r.Priced {
		t.Error("unknown model should not be marked priced")
	}
	if r.CostUSD != 0 {
		t.Errorf("unknown model cost = %v, want 0", r.CostUSD)
	}
}

func TestNormalizeStripsSnapshotAndProviderDecoration(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"anthropic.claude-opus-5", "claude-opus-5"},
		{"  Claude-Opus-5  ", "claude-opus-5"},
		{"claude-opus-5", "claude-opus-5"},
	} {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if !Priced("claude-haiku-4-5-20251001") {
		t.Error("a dated snapshot id should price like its alias")
	}
}

const transcript = `
{"type":"user","message":{"content":"hi"}}
{"type":"assistant","timestamp":"2026-07-25T07:00:00.000Z","uuid":"u1","message":{"id":"msg_A","model":"claude-opus-5","usage":{"input_tokens":2,"output_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":7264,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":7264},"speed":"standard"}}}
{"type":"assistant","timestamp":"2026-07-25T07:00:01.000Z","uuid":"u2","message":{"id":"msg_A","model":"claude-opus-5","usage":{"input_tokens":2,"output_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":7264,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":7264},"speed":"standard"}}}
{"type":"assistant","timestamp":"2026-07-25T07:00:02.000Z","uuid":"u3","isSidechain":true,"message":{"id":"msg_B","model":"claude-opus-5","usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":900}}}
not json at all
{"type":"assistant","message":{"id":"msg_C","model":"claude-opus-5"}}
`

func TestParseDeduplicatesByMessageID(t *testing.T) {
	// The transcript rewrites an assistant message across several lines, each
	// carrying the same final usage. Counting lines instead of messages
	// overstated output tokens by 2.2x on a real transcript.
	recs, err := ParseClaudeCode(strings.NewReader(transcript), "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (msg_A once, msg_B once)", len(recs))
	}
	var out int64
	for _, r := range recs {
		out += r.OutputTokens
	}
	if out != 107 {
		t.Errorf("output tokens = %d, want 107 — the duplicate line was counted twice", out)
	}
}

func TestParseSplitsCacheWriteTiersAndFallsBack(t *testing.T) {
	recs, _ := ParseClaudeCode(strings.NewReader(transcript), "me")
	byID := map[string]Record{}
	for _, r := range recs {
		byID[r.ExternalID] = r
	}
	if a := byID["msg_A"]; a.CacheWrite1h != 7264 || a.CacheWrite5m != 0 {
		t.Errorf("msg_A tiers = 5m:%d 1h:%d, want 5m:0 1h:7264", a.CacheWrite5m, a.CacheWrite1h)
	}
	// No breakdown present: attribute to the cheaper tier rather than inventing
	// an expensive one.
	if b := byID["msg_B"]; b.CacheWrite5m != 900 || b.CacheWrite1h != 0 {
		t.Errorf("msg_B tiers = 5m:%d 1h:%d, want 5m:900 1h:0", b.CacheWrite5m, b.CacheWrite1h)
	}
}

func TestParseCapturesSubagentDimensionAndSurvivesJunk(t *testing.T) {
	recs, err := ParseClaudeCode(strings.NewReader(transcript), "me")
	if err != nil {
		t.Fatalf("a malformed line must not abort the parse: %v", err)
	}
	var sub int
	for _, r := range recs {
		if r.Subagent {
			sub++
		}
		if r.Actor != "me" || r.Source != SourceClaudeCode {
			t.Errorf("record not attributed: %+v", r)
		}
	}
	if sub != 1 {
		t.Errorf("subagent records = %d, want 1", sub)
	}
}
