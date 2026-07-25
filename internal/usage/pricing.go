// Package usage records what agents spend: tokens and dollars, per model, per
// participant.
//
// This is telemetry, not shared knowledge, so it lives in its own table rather
// than in the event log. Spend is not something one agent needs to read out of
// another's context, and putting it in the log would burn catchup budget on
// rows nobody reads.
package usage

import (
	"regexp"
	"strings"
	"time"
)

// Rate is USD per million tokens.
type Rate struct {
	Input  float64
	Output float64
}

// Cache pricing is expressed as multipliers on the model's input rate rather
// than as separate per-model numbers, because that is how it is actually
// defined — a 1h cache write costs 2x input on every model.
const (
	CacheReadMultiplier    = 0.10
	CacheWrite5mMultiplier = 1.25
	CacheWrite1hMultiplier = 2.00
)

var standardRates = map[string]Rate{
	"claude-fable-5":    {10, 50},
	"claude-mythos-5":   {10, 50},
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-opus-4-7":   {5, 25},
	"claude-opus-4-6":   {5, 25},
	"claude-opus-4-5":   {5, 25},
	"claude-opus-4-1":   {15, 75},
	"claude-sonnet-5":   {3, 15},
	"claude-sonnet-4-6": {3, 15},
	"claude-sonnet-4-5": {3, 15},
	"claude-haiku-4-5":  {1, 5},
}

// fastRates apply when the response reports speed="fast". Fast mode is a
// different price, not a different model, so it cannot be inferred from the
// model id alone.
var fastRates = map[string]Rate{
	"claude-opus-5":   {10, 50},
	"claude-opus-4-8": {10, 50},
}

// introRates are promotional prices with an end date. They are applied against
// the event's own timestamp, not the current time, so re-importing an old
// transcript reproduces what it actually cost.
var introRates = map[string]struct {
	Rate
	Until time.Time
}{
	"claude-sonnet-5": {Rate{2, 10}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// Normalize strips a dated snapshot suffix and any provider prefix so that
// "anthropic.claude-haiku-4-5-20251001" prices the same as "claude-haiku-4-5".
func Normalize(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	m = strings.TrimPrefix(m, "anthropic.")
	m = strings.TrimSuffix(m, "[1m]")
	return dateSuffix.ReplaceAllString(m, "")
}

// Lookup returns the rate for a model at a point in time. The bool reports
// whether the model is priced at all: an unknown model must show up as
// unpriced in a dashboard rather than silently costing zero.
func Lookup(model, speed string, at time.Time) (Rate, bool) {
	m := Normalize(model)
	if speed == "fast" {
		if r, ok := fastRates[m]; ok {
			return r, true
		}
	}
	if intro, ok := introRates[m]; ok && at.Before(intro.Until) {
		return intro.Rate, true
	}
	r, ok := standardRates[m]
	return r, ok
}

// Priced reports whether a model has a known rate at all, ignoring time.
func Priced(model string) bool {
	_, ok := standardRates[Normalize(model)]
	return ok
}
