package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lll-lll-lll-lll/copresence/internal/event"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), DirName, "session.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func post(t *testing.T, st *Store, actor string, typ event.Type, subject, body string, refs ...string) int64 {
	t.Helper()
	seq, err := st.Post(context.Background(), &event.Event{
		Session: "main", Actor: actor, Type: typ, Subject: subject, Body: body, Refs: refs,
	})
	if err != nil {
		t.Fatalf("Post(%s): %v", typ, err)
	}
	return seq
}

func seqs(evs []event.Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func TestUnreadExcludesOwnEventsAndAdvancesByWatermark(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	post(t, st, "a", event.Finding, "", "from a")
	mine := post(t, st, "me", event.Finding, "", "from me")
	third := post(t, st, "b", event.Finding, "", "from b")

	got, err := st.Unread(ctx, "main", "me", 1<<62, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unread = %v, want the two events not written by me", seqs(got))
	}
	for _, e := range got {
		if e.Seq == mine {
			t.Error("a participant must not be served its own events back")
		}
	}

	if err := st.Advance(ctx, "main", "me", third); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Unread(ctx, "main", "me", 1<<62, 0); len(got) != 0 {
		t.Errorf("after advancing to head, unread = %v, want empty", seqs(got))
	}
}

func TestAdvanceNeverMovesBackward(t *testing.T) {
	// Two catchups racing must not cause events to be replayed.
	ctx := context.Background()
	st := open(t)
	if err := st.Advance(ctx, "main", "me", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Advance(ctx, "main", "me", 40); err != nil {
		t.Fatal(err)
	}
	if wm, _ := st.Watermark(ctx, "main", "me"); wm != 100 {
		t.Errorf("watermark = %d, want it to stay at 100", wm)
	}
}

func TestSupersededEventsDisappearFromEveryReadPath(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	wrong := post(t, st, "a", event.Finding, "src/auth/jwt.go", "exp is never checked")
	if _, err := st.Post(ctx, &event.Event{
		Session: "main", Actor: "a", Type: event.Finding, Subject: "src/auth/jwt.go",
		Body: "correction: exp is checked in middleware", Supersedes: wrong,
	}); err != nil {
		t.Fatal(err)
	}

	checks := map[string]func() ([]event.Event, error){
		"Unread":        func() ([]event.Event, error) { return st.Unread(ctx, "main", "me", 1<<62, 0) },
		"Search":        func() ([]event.Event, error) { return st.Search(ctx, "main", "exp", "", "", 0) },
		"ContextFor":    func() ([]event.Event, error) { return st.ContextFor(ctx, "main", "src/auth/jwt.go", 0) },
		"ByType":        func() ([]event.Event, error) { return st.ByType(ctx, "main", event.Finding) },
		"Since":         func() ([]event.Event, error) { return st.Since(ctx, "main", 0, 0) },
		"OpenQuestions": func() ([]event.Event, error) { return st.OpenQuestions(ctx, "main") },
	}
	for name, fn := range checks {
		evs, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, e := range evs {
			if e.Seq == wrong {
				t.Errorf("%s still serves superseded event #%d", name, wrong)
			}
		}
	}
}

func TestOpenQuestionsCloseOnlyOnAnAnswerThatRefsThem(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	q1 := post(t, st, "a", event.Question, "", "how does refresh expiry work?")
	q2 := post(t, st, "b", event.Question, "", "is the migration order fixed?")

	// An answer aimed at q2 must not close q1.
	post(t, st, "c", event.Answer, "", "this one is about the migration", "event:"+itoa(q2))
	open, err := st.OpenQuestions(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Seq != q1 {
		t.Fatalf("open = %v, want only #%d", seqs(open), q1)
	}
	// And an answer pointing nowhere must not post at all.
	if _, err := st.Post(ctx, &event.Event{
		Session: "main", Actor: "c", Type: event.Answer, Body: "x", Refs: []string{"event:9999"},
	}); err == nil {
		t.Error("expected an answer reffing a nonexistent event to be rejected")
	}

	post(t, st, "c", event.Answer, "", "it is handled in middleware", "event:"+itoa(q1))
	if open, _ := st.OpenQuestions(ctx, "main"); len(open) != 0 {
		t.Errorf("open = %v, want none", seqs(open))
	}
}

func TestRetractingAnAnswerReopensTheQuestion(t *testing.T) {
	// supersedes is documented to retire an event on every read path. This was
	// the one path where it did not hold: the question stayed closed by an
	// answer that no longer existed.
	ctx := context.Background()
	st := open(t)
	q := post(t, st, "a", event.Question, "", "does this reopen?")
	ans := post(t, st, "b", event.Answer, "", "a wrong answer", "event:"+itoa(q))
	if got, _ := st.OpenQuestions(ctx, "main"); len(got) != 0 {
		t.Fatalf("precondition: question should be closed, got %v", seqs(got))
	}

	if _, err := st.Post(ctx, &event.Event{
		Session: "main", Actor: "b", Type: event.Note, Body: "retracting that answer", Supersedes: ans,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.OpenQuestions(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != q {
		t.Errorf("open = %v, want #%d reopened", seqs(got), q)
	}
}

func TestSupersedesMustExistInTheSameSession(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	target := post(t, st, "a", event.Finding, "", "in main")

	if _, err := st.Post(ctx, &event.Event{
		Session: "main", Actor: "a", Type: event.Note, Body: "x", Supersedes: 99999,
	}); err == nil {
		t.Error("expected a dangling supersedes to be rejected")
	}
	// Cross-session retirement is the dangerous one: seq numbers are global, so
	// a correction in a scratch session could silently retire real work.
	if _, err := st.Post(ctx, &event.Event{
		Session: "spike", Actor: "a", Type: event.Note, Body: "x", Supersedes: target,
	}); err == nil {
		t.Error("expected supersedes across sessions to be rejected")
	}
	if got, _ := st.Since(ctx, "main", 0, 0); len(got) != 1 {
		t.Errorf("event in main was retired from another session: %v", seqs(got))
	}
}

func TestRefsAreCanonicalizedSoAnswersActuallyClose(t *testing.T) {
	// "event: 001" validates and posts; if it is not canonicalized it closes
	// nothing, which reads as resolved while the question stays open forever.
	ctx := context.Background()
	st := open(t)
	q := post(t, st, "a", event.Question, "", "will a sloppy ref close me?")
	post(t, st, "b", event.Answer, "", "yes", "event: 00"+itoa(q))
	if got, _ := st.OpenQuestions(ctx, "main"); len(got) != 0 {
		t.Errorf("open = %v, want the question closed by the normalized ref", seqs(got))
	}
}

func TestSearchNeverReturnsAnErrorForModelWrittenQueries(t *testing.T) {
	// A model searching the log will type a file path, and a path is a string
	// full of FTS5 operators. Every one of these used to be a SQL error.
	ctx := context.Background()
	st := open(t)
	post(t, st, "a", event.Finding, "src/auth/jwt.go:88", "the exp claim is never validated")
	post(t, st, "b", event.Finding, "", "書き忘れ問題はエージェントが明示 post しないと起きる")

	for _, q := range []string{
		"src/auth/jwt.go", "queries.go:87", "auth:", "-foo", "what is this?", "AND", "*", "\"", "a", "(x OR",
	} {
		if _, err := st.Search(ctx, "main", q, "", "", 0); err != nil {
			t.Errorf("Search(%q) returned an error: %v", q, err)
		}
	}

	// CJK must be searchable: the tokenizer choice is what makes "folded detail
	// is recoverable through search" true for a Japanese log.
	for _, q := range []string{"書き忘れ", "エージェント", "exp claim"} {
		got, err := st.Search(ctx, "main", q, "", "", 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(got) == 0 {
			t.Errorf("Search(%q) found nothing", q)
		}
	}
}

func TestAnswerWithoutARefIsRejected(t *testing.T) {
	// An answer that does not close anything leaves the question open forever
	// while looking resolved to a human reader.
	_, err := open(t).Post(context.Background(), &event.Event{
		Session: "main", Actor: "a", Type: event.Answer, Body: "sure, it works",
	})
	if err == nil {
		t.Fatal("expected an answer with no event: ref to be rejected")
	}
}

func TestContextForMatchesPathPrefixAndFileRefs(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	exact := post(t, st, "a", event.Finding, "src/auth/jwt.go:88", "exp not checked")
	deeper := post(t, st, "a", event.Finding, "src/auth/session.go", "cookie flags")
	viaRef := post(t, st, "b", event.Note, "", "touched during refactor", "file:src/auth/jwt.go")
	other := post(t, st, "a", event.Finding, "docs/readme.md", "unrelated")

	got, err := st.ContextFor(ctx, "main", "src/auth", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]bool{}
	for _, e := range got {
		found[e.Seq] = true
	}
	if !found[exact] || !found[deeper] {
		t.Errorf("prefix lookup missed events: got %v", seqs(got))
	}
	if found[other] {
		t.Error("unrelated subject leaked into context")
	}

	got, _ = st.ContextFor(ctx, "main", "src/auth/jwt.go", 0)
	found = map[int64]bool{}
	for _, e := range got {
		found[e.Seq] = true
	}
	if !found[viaRef] {
		t.Errorf("file: ref was not matched: got %v", seqs(got))
	}
}

func TestSearchFiltersAndFullText(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	post(t, st, "a", event.Finding, "src/auth/jwt.go", "the exp claim is never validated")
	post(t, st, "b", event.Decision, "", "use SQLite because a daemon is overkill")
	post(t, st, "c", event.Note, "", "unrelated chatter")

	if got, _ := st.Search(ctx, "main", "sqlite", "", "", 0); len(got) != 1 {
		t.Errorf("fts search = %v, want 1 hit", seqs(got))
	}
	if got, _ := st.Search(ctx, "main", "", "decision", "", 0); len(got) != 1 {
		t.Errorf("type filter = %v, want 1 hit", seqs(got))
	}
	if got, _ := st.Search(ctx, "main", "", "", "src/", 0); len(got) != 1 {
		t.Errorf("subject prefix filter = %v, want 1 hit", seqs(got))
	}
}

func TestLatestStatusIsOnePerActorExcludingSelf(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	post(t, st, "a", event.Status, "", "reading auth")
	newest := post(t, st, "a", event.Status, "", "now writing tests")
	post(t, st, "me", event.Status, "", "my own status")
	post(t, st, "b", event.Status, "", "on migrations")

	got, err := st.LatestStatus(ctx, "main", "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("statuses = %v, want one per other actor", seqs(got))
	}
	for _, e := range got {
		if e.Actor == "me" {
			t.Error("own status must be excluded")
		}
		if e.Actor == "a" && e.Seq != newest {
			t.Errorf("got stale status #%d for actor a, want #%d", e.Seq, newest)
		}
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	post(t, st, "a", event.Finding, "", "in main")
	if _, err := st.Post(ctx, &event.Event{Session: "spike", Actor: "a", Type: event.Finding, Body: "in spike"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Unread(ctx, "main", "me", 1<<62, 0); len(got) != 1 {
		t.Errorf("main leaked events from spike: %v", seqs(got))
	}
	if got, _ := st.Unread(ctx, "spike", "me", 1<<62, 0); len(got) != 1 {
		t.Errorf("spike unread = %v, want 1", seqs(got))
	}
}

func TestValidationRejectsUnknownTypeAndEmptyBody(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if _, err := st.Post(ctx, &event.Event{Session: "main", Actor: "a", Type: "rumour", Body: "x"}); err == nil {
		t.Error("expected unknown type to be rejected")
	}
	if _, err := st.Post(ctx, &event.Event{Session: "main", Actor: "a", Type: event.Note, Body: ""}); err == nil {
		t.Error("expected empty body to be rejected")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
