package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/event"
	"github.com/lll-lll-lll-lll/copresence/internal/store"
	"github.com/lll-lll-lll-lll/copresence/internal/usage"
)

const workspace = "/work/proj"

func newServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, Config{Session: "main", Workspace: workspace}), st
}

func post(t *testing.T, st *store.Store, e *event.Event) int64 {
	t.Helper()
	e.Session = "main"
	seq, err := st.Post(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// get drives the full handler chain, including the localhost guard, so the
// tests exercise what a browser actually reaches.
func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:8787"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func getState(t *testing.T, s *Server, target string) State {
	t.Helper()
	w := get(t, s, target)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body.String())
	}
	var st State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRetiredEventsDoNotReachThePage(t *testing.T) {
	// supersedes is the mechanism that keeps a wrong finding from haunting a
	// session. A dashboard that renders the raw table would undo that in the
	// one place a human is most likely to be reading.
	s, st := newServer(t)
	wrong := post(t, st, &event.Event{Actor: "a", Type: event.Finding, Body: "auth uses sessions"})
	post(t, st, &event.Event{Actor: "a", Type: event.Finding, Body: "auth uses JWT", Supersedes: wrong})

	got := getState(t, s, "/api/state")
	for _, e := range got.Timeline {
		if e.Seq == wrong {
			t.Fatalf("retired event #%d is on the timeline", wrong)
		}
	}
	if len(got.Timeline) != 1 {
		t.Fatalf("timeline has %d events, want 1", len(got.Timeline))
	}
	if got.Head != 2 {
		t.Errorf("head = #%d, want #2 — the head is the log's, not the projection's", got.Head)
	}
}

func TestAnsweredQuestionsLeaveTheOpenPane(t *testing.T) {
	s, st := newServer(t)
	q1 := post(t, st, &event.Event{Actor: "a", Type: event.Question, Body: "which store?"})
	post(t, st, &event.Event{Actor: "b", Type: event.Question, Body: "which port?"})
	post(t, st, &event.Event{Actor: "b", Type: event.Answer, Body: "sqlite",
		Refs: []string{"event:" + itoa(q1)}})

	got := getState(t, s, "/api/state")
	if len(got.Questions) != 1 || got.Questions[0].Body != "which port?" {
		t.Fatalf("open questions = %+v, want only the unanswered one", got.Questions)
	}
}

func TestParticipantProgressIsReported(t *testing.T) {
	// "who is behind" is the question the participants pane exists to answer;
	// if unread is wrong the pane is decorative.
	s, st := newServer(t)
	ctx := context.Background()
	for _, id := range []string{"reader", "writer"} {
		if err := st.Join(ctx, "main", id, "", "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	post(t, st, &event.Event{Actor: "writer", Type: event.Note, Body: "one"})
	seq := post(t, st, &event.Event{Actor: "writer", Type: event.Note, Body: "two"})
	if err := st.Advance(ctx, "main", "reader", seq-1); err != nil {
		t.Fatal(err)
	}

	byID := map[string]Participant{}
	for _, p := range getState(t, s, "/api/state").Participants {
		byID[p.ID] = p
	}
	if got := byID["reader"]; got.Unread != 1 || got.ReadTo != seq-1 {
		t.Errorf("reader = %d unread, read-to #%d; want 1 unread, read-to #%d", got.Unread, got.ReadTo, seq-1)
	}
	// A writer is never behind on its own events: they are filtered from every
	// unread path so the log does not echo an agent back to itself.
	if got := byID["writer"]; got.Unread != 0 || got.Posted != 2 {
		t.Errorf("writer = %d unread, %d posted; want 0 unread, 2 posted", got.Unread, got.Posted)
	}
}

func TestSpendIsScopedToTheWorkspaceUnlessAsked(t *testing.T) {
	// A transcript directory can span sibling projects. Defaulting to every
	// project would show this workspace a total it did not spend.
	s, st := newServer(t)
	recs := []usage.Record{
		mkRecord("here", workspace, 1),
		mkRecord("nested", workspace+"/internal", 2),
		mkRecord("elsewhere", "/work/other", 3),
	}
	if _, err := st.RecordUsage(context.Background(), "main", recs); err != nil {
		t.Fatal(err)
	}

	scoped := getState(t, s, "/api/state").Usage
	if scoped.Total.Calls != 2 {
		t.Errorf("scoped calls = %d, want 2 (the sibling project must not count)", scoped.Total.Calls)
	}
	if scoped.Scope != workspace {
		t.Errorf("scope = %q, want the workspace echoed back", scoped.Scope)
	}
	all := getState(t, s, "/api/state?all_projects=1").Usage
	if all.Total.Calls != 3 {
		t.Errorf("all-projects calls = %d, want 3", all.Total.Calls)
	}
	if len(all.ByProject) != 3 {
		t.Errorf("by_project = %d rows, want one per directory", len(all.ByProject))
	}
}

func TestDayAxisIsChronologicalNotRanked(t *testing.T) {
	// Every other dimension is ranked by cost. A day axis ranked by cost draws
	// a chart whose bars are in the wrong order — and it looks plausible.
	s, st := newServer(t)
	var recs []usage.Record
	for i, day := range []int{3, 1, 2} {
		r := mkRecord("a", workspace, int64(i+1))
		r.TS = time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC)
		r.OutputTokens = int64(day) * 1000 // deliberately not in date order
		r.Cost()
		recs = append(recs, r)
	}
	if _, err := st.RecordUsage(context.Background(), "main", recs); err != nil {
		t.Fatal(err)
	}
	days := getState(t, s, "/api/state").Usage.ByDay
	if len(days) != 3 {
		t.Fatalf("got %d days, want 3", len(days))
	}
	for i := 1; i < len(days); i++ {
		if days[i-1].Key >= days[i].Key {
			t.Fatalf("days out of order: %v", []string{days[0].Key, days[1].Key, days[2].Key})
		}
	}
}

func TestSpendAttachesToAParticipantOnlyOnAnExactMatch(t *testing.T) {
	// Transcript actors are derived from file names unless the agent imported
	// under its own --as. Guessing at the mapping would put a real number next
	// to the wrong name, which is worse than putting none.
	s, st := newServer(t)
	ctx := context.Background()
	if err := st.Join(ctx, "main", "claude-1", "", "claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := st.Join(ctx, "main", "codex-1", "", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordUsage(ctx, "main", []usage.Record{mkRecord("claude-1", workspace, 1)}); err != nil {
		t.Fatal(err)
	}
	for _, p := range getState(t, s, "/api/state").Participants {
		switch p.ID {
		case "claude-1":
			if p.Spend == nil {
				t.Error("claude-1 has usage under its own id and should show spend")
			}
		case "codex-1":
			if p.Spend != nil {
				t.Errorf("codex-1 has no usage rows but was given spend %+v", p.Spend)
			}
		}
	}
}

func TestForeignHostIsRefused(t *testing.T) {
	// DNS rebinding: the attacker's name resolves to 127.0.0.1, so the request
	// arrives here, but the browser still sends their hostname in Host.
	s, _ := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "evil.example.com"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("Host: evil.example.com = %d, want 403", w.Code)
	}
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	s, _ := newServer(t)
	for _, site := range []string{"cross-site", "same-site"} {
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = "localhost:8787"
		req.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %s = %d, want 403", site, w.Code)
		}
	}
	// The page's own fetch must still work.
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "localhost:8787"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestWritesAreNotRoutedAtAll(t *testing.T) {
	// The dashboard is read-only by construction: there is no handler to reach.
	s, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("POST /api/state was accepted; the dashboard must not write")
	}
}

func TestBadParametersAreClientErrors(t *testing.T) {
	s, _ := newServer(t)
	for _, target := range []string{"/api/state?type=nonsense", "/api/state?since=yesterday"} {
		if got := get(t, s, target).Code; got != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, got)
		}
	}
}

func TestTimelineLimitIsBounded(t *testing.T) {
	// The limit comes from a query string; an unbounded one turns a URL into a
	// way to read the whole log into memory.
	s, st := newServer(t)
	for i := 0; i < 5; i++ {
		post(t, st, &event.Event{Actor: "a", Type: event.Note, Body: "n"})
	}
	if n := len(getState(t, s, "/api/state?limit=2").Timeline); n != 2 {
		t.Errorf("limit=2 returned %d events", n)
	}
	if got := clampLimit("999999"); got != maxTimeline {
		t.Errorf("clampLimit(999999) = %d, want %d", got, maxTimeline)
	}
	if got := clampLimit("-1"); got != defaultTimeline {
		t.Errorf("clampLimit(-1) = %d, want the default %d", got, defaultTimeline)
	}
}

func TestIndexAndAssetsAreServed(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := get(t, s, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)
		}
		if w.Body.Len() == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
}

func mkRecord(actor, cwd string, n int64) usage.Record {
	r := usage.Record{
		Actor: actor, TS: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Source: "test", ExternalID: "msg_" + itoa(n), Model: "claude-opus-5",
		CWD: cwd, InputTokens: 100, OutputTokens: 100,
	}
	r.Cost()
	return r
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRunFilterNarrowsSpendWithoutHidingTheOtherRuns(t *testing.T) {
	// Picking a run must not collapse the list you picked from — otherwise
	// there is no way back to the other runs except editing the URL.
	s, st := newServer(t)
	recs := []usage.Record{
		withRun(mkRecord("a", workspace, 1), "4afcba68-e360"),
		withRun(mkRecord("a", workspace, 2), "4afcba68-e360"),
		withRun(mkRecord("a", workspace, 3), "97dfea59-8a4b"),
	}
	if _, err := st.RecordUsage(context.Background(), "main", recs); err != nil {
		t.Fatal(err)
	}

	u := getState(t, s, "/api/state?run=4afcba68").Usage
	if u.Total.Calls != 2 {
		t.Errorf("filtered total = %d calls, want 2", u.Total.Calls)
	}
	if u.Run != "4afcba68" {
		t.Errorf("run = %q, want the filter echoed back so the page can say what it covers", u.Run)
	}
	if len(u.ByRun) != 2 {
		t.Errorf("by_run = %d rows, want both runs still listed", len(u.ByRun))
	}
	if len(u.ByActor) != 1 || u.ByActor[0].Calls != 2 {
		t.Errorf("by_actor = %+v, want it narrowed to the picked run", u.ByActor)
	}
}

func TestRunFilterLeavesTheSessionPanesAlone(t *testing.T) {
	// Events carry no run id. Silently emptying the timeline when a run is
	// picked would read as "this run did nothing" rather than "this filter does
	// not apply here".
	s, st := newServer(t)
	post(t, st, &event.Event{Actor: "a", Type: event.Decision, Body: "use sqlite"})
	if _, err := st.RecordUsage(context.Background(), "main",
		[]usage.Record{withRun(mkRecord("a", workspace, 1), "4afcba68")}); err != nil {
		t.Fatal(err)
	}
	got := getState(t, s, "/api/state?run=nomatch")
	if len(got.Timeline) != 1 || len(got.Decisions) != 1 {
		t.Errorf("a run filter emptied the event panes: timeline=%d decisions=%d",
			len(got.Timeline), len(got.Decisions))
	}
	if got.Usage.Total.Calls != 0 {
		t.Errorf("spend = %d calls, want 0 for a run that does not exist", got.Usage.Total.Calls)
	}
}

func TestRunRowsCarryTheirTimeSpan(t *testing.T) {
	// The UI labels each run by when it ran; a run id alone identifies nothing.
	s, st := newServer(t)
	var recs []usage.Record
	for i, h := range []int{9, 17} {
		r := withRun(mkRecord("a", workspace, int64(i+1)), "4afcba68")
		r.TS = time.Date(2026, 7, 25, h, 0, 0, 0, time.UTC)
		recs = append(recs, r)
	}
	if _, err := st.RecordUsage(context.Background(), "main", recs); err != nil {
		t.Fatal(err)
	}
	rows := getState(t, s, "/api/state").Usage.ByRun
	if len(rows) != 1 {
		t.Fatalf("got %d runs, want 1", len(rows))
	}
	if rows[0].First.UTC().Hour() != 9 || rows[0].Last.UTC().Hour() != 17 {
		t.Errorf("span = %s → %s, want 09:00 → 17:00", rows[0].First, rows[0].Last)
	}
}

func withRun(r usage.Record, run string) usage.Record {
	r.RunID = run
	return r
}
