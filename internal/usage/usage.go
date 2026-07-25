package usage

import "time"

// Record is one billable model call.
type Record struct {
	ID      int64     `json:"id"`
	Session string    `json:"session"`
	Actor   string    `json:"actor"`
	TS      time.Time `json:"ts"`
	// Source names the importer ("claude-code"), and ExternalID is that
	// source's identifier for the call. Together they are unique, which is what
	// makes re-importing a growing transcript safe.
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`

	Model string `json:"model"`
	Speed string `json:"speed,omitempty"`
	// CWD is the directory the agent was working in. It is the only unit of
	// work available for free: the agent does not have to declare it, it is a
	// byproduct of doing the work at all. Signals that must be declared — a
	// status, an explicit work marker — are the ones that go unposted.
	CWD string `json:"cwd,omitempty"`
	// Subagent marks work done by a delegated agent rather than the main loop.
	// Kept as a dimension because "how much are subagents costing me" is the
	// first question anyone asks of this data.
	Subagent bool `json:"subagent"`

	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	CacheWrite5m    int64   `json:"cache_write_5m_tokens"`
	CacheWrite1h    int64   `json:"cache_write_1h_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	// Priced is false when the model had no known rate. CostUSD is then 0, and
	// a dashboard must show the gap rather than report a total that quietly
	// excludes it.
	Priced bool `json:"priced"`
}

// TotalTokens is every token the call touched, cached or not.
func (r Record) TotalTokens() int64 {
	return r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWrite5m + r.CacheWrite1h
}

// Cost computes and stores the price of this record. It is called once at
// import time and the result is persisted, so a later price change does not
// silently rewrite history.
func (r *Record) Cost() {
	rate, ok := Lookup(r.Model, r.Speed, r.TS)
	r.Priced = ok
	if !ok {
		r.CostUSD = 0
		return
	}
	perToken := func(n int64, usdPerMillion float64) float64 {
		return float64(n) * usdPerMillion / 1_000_000
	}
	r.CostUSD = perToken(r.InputTokens, rate.Input) +
		perToken(r.OutputTokens, rate.Output) +
		perToken(r.CacheReadTokens, rate.Input*CacheReadMultiplier) +
		perToken(r.CacheWrite5m, rate.Input*CacheWrite5mMultiplier) +
		perToken(r.CacheWrite1h, rate.Input*CacheWrite1hMultiplier)
}

// Summary aggregates records along one dimension for reporting.
type Summary struct {
	Key             string  `json:"key"`
	Calls           int64   `json:"calls"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	CacheWriteToken int64   `json:"cache_write_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	UnpricedCalls   int64   `json:"unpriced_calls"`
}
