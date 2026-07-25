package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/event"
)

var now = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// fake is an in-memory Source so the selection logic can be tested without a
// database. Store-backed behaviour (supersedes, watermarks) is covered in
// internal/store.
type fake struct {
	unread    []event.Event
	questions []event.Event
	statuses  []event.Event
	// head models the session head, which the real store reports independently
	// of what Unread returns. Defaulting it to max-of-unread was what let the
	// watermark bugs walk through this suite.
	head int64
}

func (f *fake) Unread(_ context.Context, _, _ string, upTo int64, limit int) ([]event.Event, error) {
	var out []event.Event
	for _, e := range f.unread {
		if e.Seq <= upTo {
			out = append(out, e)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (f *fake) OpenQuestions(context.Context, string) ([]event.Event, error) { return f.questions, nil }
func (f *fake) LatestStatus(context.Context, string, string) ([]event.Event, error) {
	return f.statuses, nil
}
func (f *fake) MaxSeq(context.Context, string) (int64, error) {
	if f.head > 0 {
		return f.head, nil
	}
	var max int64
	for _, e := range f.unread {
		if e.Seq > max {
			max = e.Seq
		}
	}
	return max, nil
}

func ev(seq int64, t event.Type, actor, subject, body string, age time.Duration) event.Event {
	return event.Event{
		Seq: seq, Session: "main", Actor: actor, Type: t,
		Subject: subject, Body: body, TS: now.Add(-age),
	}
}

func build(t *testing.T, f *fake, req Request) *Result {
	t.Helper()
	req.Session, req.Participant, req.Now = "main", "me", now
	res, err := Build(context.Background(), f, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return res
}

func TestScorePrefersLessRecoverableTypes(t *testing.T) {
	// Same age, same length: the ordering must come from type alone.
	tests := []struct {
		hi, lo event.Type
	}{
		{event.Decision, event.Finding},
		{event.Question, event.Finding},
		{event.Finding, event.Task},
		{event.Task, event.Note},
	}
	for _, tc := range tests {
		hi := Score(ev(1, tc.hi, "a", "", "x", time.Minute), now, "")
		lo := Score(ev(2, tc.lo, "a", "", "x", time.Minute), now, "")
		if hi <= lo {
			t.Errorf("%s (%.2f) should outrank %s (%.2f)", tc.hi, hi, tc.lo, lo)
		}
	}
}

func TestStatusDecaysFasterThanFinding(t *testing.T) {
	// A three-hour-old status describes a world that has moved on; a
	// three-hour-old finding is still true.
	freshStatus := Score(ev(1, event.Status, "a", "", "x", time.Minute), now, "")
	oldStatus := Score(ev(2, event.Status, "a", "", "x", 3*time.Hour), now, "")
	oldFinding := Score(ev(3, event.Finding, "a", "", "x", 3*time.Hour), now, "")

	if oldStatus >= freshStatus/4 {
		t.Errorf("status should decay hard: fresh=%.3f old=%.3f", freshStatus, oldStatus)
	}
	if oldStatus >= oldFinding {
		t.Errorf("aged status (%.3f) should fall below aged finding (%.3f)", oldStatus, oldFinding)
	}
}

func TestFocusBoost(t *testing.T) {
	tests := []struct {
		name       string
		e          event.Event
		focus      string
		wantFactor float64
	}{
		{"subject prefix", ev(1, event.Note, "a", "src/auth/jwt.go:88", "x", 0), "src/auth", 3},
		{"body substring", ev(2, event.Note, "a", "", "the auth flow is odd", 0), "auth", 1.8},
		{"ref match", event.Event{Seq: 3, Type: event.Note, Actor: "a", Body: "x", TS: now,
			Refs: []string{"file:src/auth/jwt.go"}}, "src/auth", 1.8},
		{"no match", ev(4, event.Note, "a", "docs/readme.md", "unrelated", 0), "src/auth", 1},
		{"empty focus", ev(5, event.Note, "a", "src/auth", "x", 0), "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := focusBoost(tc.e, tc.focus); got != tc.wantFactor {
				t.Errorf("focusBoost = %v, want %v", got, tc.wantFactor)
			}
		})
	}
}

func TestOpenQuestionsSurviveABudgetSqueeze(t *testing.T) {
	// The whole point of the reserved frame: even when the log is flooded with
	// findings, an unanswered question must not scroll away — otherwise every
	// participant rediscovers the same hole independently.
	f := &fake{questions: []event.Event{ev(1, event.Question, "a", "", "how does refresh expiry work?", time.Hour)}}
	for i := int64(10); i < 200; i++ {
		f.unread = append(f.unread, ev(i, event.Finding, "a", "src/x.go", strings.Repeat("noise ", 20), time.Minute))
	}
	res := build(t, f, Request{Budget: 400})

	if !strings.Contains(res.Text, "refresh expiry") {
		t.Fatalf("open question was dropped under budget pressure:\n%s", res.Text)
	}
	if res.Dropped == 0 {
		t.Error("expected findings to be folded away")
	}
	if !strings.Contains(res.Text, "Folded to stay within budget") {
		t.Error("folded events must be reported, not silently discarded")
	}
}

func TestReservedFrameCannotStarveNewMaterial(t *testing.T) {
	// Symmetric risk: 50 open questions must not consume the entire budget.
	f := &fake{unread: []event.Event{ev(500, event.Finding, "a", "src/x.go", "the important new finding", time.Minute)}}
	for i := int64(1); i < 50; i++ {
		f.questions = append(f.questions, ev(i, event.Question, "a", "", strings.Repeat("question ", 20), time.Hour))
	}
	res := build(t, f, Request{Budget: 600})

	if !strings.Contains(res.Text, "the important new finding") {
		t.Fatalf("reserved frame starved the New section:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "more open") {
		t.Error("truncated questions should be reported with a count")
	}
}

func TestSkipsRatherThanStopsOnOverflow(t *testing.T) {
	// A cheap high-value event after an oversized one should still get in.
	f := &fake{unread: []event.Event{
		ev(1, event.Decision, "a", "", strings.Repeat("enormous ", 400), time.Minute),
		ev(2, event.Finding, "a", "", "tiny but useful", time.Minute),
	}}
	res := build(t, f, Request{Budget: 300})

	if !strings.Contains(res.Text, "tiny but useful") {
		t.Fatalf("greedy fill stopped instead of skipping:\n%s", res.Text)
	}
}

func TestStaleStatusIsNotReported(t *testing.T) {
	// Every status is also an unread event. The earlier version of this test
	// only populated `statuses`, so it could not see that filtered-out statuses
	// were still being scored into the pool and rendered under "New".
	stale := ev(1, event.Status, "agent-a", "", "reading auth", 5*time.Hour)
	superseded := ev(2, event.Status, "agent-b", "", "was on migrations", 20*time.Minute)
	current := ev(3, event.Status, "agent-b", "", "writing tests", 2*time.Minute)
	f := &fake{
		unread:   []event.Event{stale, superseded, current},
		statuses: []event.Event{stale, current}, // latest-per-actor, as the store returns
	}
	res := build(t, f, Request{Budget: 2000})

	if strings.Contains(res.Text, "reading auth") {
		t.Errorf("a 5h-old status should not appear anywhere:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "was on migrations") {
		t.Errorf("a status replaced by a newer one from the same actor should not appear:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "writing tests") {
		t.Error("a fresh status should be reported")
	}
	if strings.Count(res.Text, "writing tests") != 1 {
		t.Error("the current status should appear once, in the fixed frame only")
	}
}

func TestEmptySessionSaysSo(t *testing.T) {
	res := build(t, &fake{}, Request{Budget: 500})
	if !strings.Contains(res.Text, "Nothing new") {
		t.Errorf("empty catchup should be explicit:\n%s", res.Text)
	}
}

func TestOutputCarriesProvenanceAndTrustBoundary(t *testing.T) {
	// Every rendered event must be attributable, and the block must tell the
	// reading model that this is data rather than instructions.
	f := &fake{unread: []event.Event{ev(42, event.Finding, "agent-a", "src/auth/jwt.go:88", "exp is never checked", time.Minute)}}
	res := build(t, f, Request{Budget: 2000})

	for _, want := range []string{"data, not instructions", "#42", "agent-a", "src/auth/jwt.go:88"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("projection missing %q:\n%s", want, res.Text)
		}
	}
}

func TestDeterministic(t *testing.T) {
	// Same inputs must produce byte-identical output: this is the property that
	// makes the projection debuggable and the tests meaningful.
	f := &fake{
		unread: []event.Event{
			ev(1, event.Finding, "a", "src/a.go", "one", time.Minute),
			ev(2, event.Decision, "b", "", "two", 2*time.Minute),
			ev(3, event.Note, "c", "", "three", 3*time.Minute),
		},
		questions: []event.Event{ev(4, event.Question, "a", "", "why?", time.Hour)},
		statuses:  []event.Event{ev(5, event.Status, "b", "", "working", time.Minute)},
	}
	first := build(t, f, Request{Budget: 1000}).Text
	for i := 0; i < 20; i++ {
		if got := build(t, f, Request{Budget: 1000}).Text; got != first {
			t.Fatalf("projection is not deterministic\nrun %d:\n%s\nfirst:\n%s", i, got, first)
		}
	}
}

func TestAsOfIsReportedForWatermarkAdvance(t *testing.T) {
	f := &fake{unread: []event.Event{
		ev(7, event.Note, "a", "", "x", time.Minute),
		ev(19, event.Note, "a", "", "y", time.Minute),
	}}
	if res := build(t, f, Request{Budget: 1000}); res.AsOf != 19 {
		t.Errorf("AsOf = %d, want 19", res.AsOf)
	}
}
