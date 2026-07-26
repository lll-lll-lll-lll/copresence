// Package dashboard serves a read-only web view of a session: who is present,
// what is still open, what was decided, and what it cost.
//
// It is deliberately not a cost dashboard. ccusage and friends already parse
// transcripts and price them, and do it across more runtimes than this project
// ever will. What none of them can show is the thing copresence owns: the spend
// next to the session it bought — the decisions, the open questions, and the
// participants that produced it.
package dashboard

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/event"
	"github.com/lll-lll-lll-lll/copresence/internal/store"
	"github.com/lll-lll-lll-lll/copresence/internal/usage"
)

//go:embed static
var staticFS embed.FS

// Config is what the server needs to know beyond the database itself.
type Config struct {
	// Session is the session name to render.
	Session string
	// Workspace is the resolved workspace root. It is both displayed and used
	// as the default cwd scope for spend, so the page cannot silently report a
	// sibling project's cost as this one's.
	Workspace string
	// AllProjects starts the page with cwd scoping off.
	AllProjects bool
}

// Server renders one session. Every route is a GET; nothing here writes to the
// log. A dashboard that can post events is a dashboard that can be made to post
// events by any page the user happens to have open.
type Server struct {
	st  *store.Store
	cfg Config
}

func New(st *store.Store, cfg Config) *Server {
	if cfg.Session == "" {
		cfg.Session = "main"
	}
	return &Server{st: st, cfg: cfg}
}

// Handler builds the routes. Exposed separately from Serve so tests can drive
// it through httptest without binding a port.
func (s *Server) Handler() http.Handler {
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // impossible: the directory is embedded at build time
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.Handle("GET /", http.FileServerFS(assets))
	return localOnly(mux)
}

// Serve runs the dashboard until ctx is cancelled.
//
// The listener is created before returning so the caller can print a URL that
// is already accepting connections, and so "port in use" surfaces as an error
// from Serve rather than as a browser tab that never loads.
func (s *Server) Serve(ctx context.Context, addr string, onReady func(url string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// A local dashboard still faces the browser, and a browser will happily
		// hold connections open. Bound the header read so a stuck tab cannot
		// pin a goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if onReady != nil {
		onReady("http://" + ln.Addr().String() + "/")
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localOnly refuses requests that did not come from a browser pointed at this
// machine.
//
// Two distinct attacks are in scope, and neither is hypothetical for a server
// that holds a workspace path and everything the agents said about the code:
//
//   - DNS rebinding. An attacker's domain re-resolves to 127.0.0.1, so the
//     request reaches this process from a page the attacker controls — but it
//     still carries their hostname in Host. Requiring a loopback Host rejects
//     it, since the browser will not forge that header.
//   - Ordinary cross-origin reads. No CORS headers are sent, so a cross-site
//     fetch cannot read the response body; Sec-Fetch-Site lets the request be
//     refused outright instead of relying on that.
//
// http.CrossOriginProtection is the standard-library tool here, but it exempts
// GET by design (safe methods must not change state), and every route on this
// server is a GET whose *response* is the thing worth protecting.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "copresence dashboard only serves localhost", http.StatusForbidden)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "none", "same-origin":
			// Absent means a non-browser client (curl, a test); allowed.
		default:
			http.Error(w, "cross-site requests are not allowed", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// State is one atomic snapshot of everything the page renders.
//
// A single endpoint rather than one per pane: the panes are read together, and
// four independent polls would let the participant list and the timeline
// disagree about what the head is.
type State struct {
	Session     string         `json:"session"`
	Workspace   string         `json:"workspace"`
	Head        int64          `json:"head"`
	GeneratedAt time.Time      `json:"generated_at"`
	Counts      map[string]int `json:"counts"`
	Types       []string       `json:"types"`

	Participants []Participant `json:"participants"`
	Questions    []event.Event `json:"questions"`
	Decisions    []event.Event `json:"decisions"`
	Timeline     []event.Event `json:"timeline"`
	Usage        Usage         `json:"usage"`
}

// Participant is a session member plus the two numbers that say whether the
// shared log is actually working for them: how far behind they are, and how
// much they have contributed.
type Participant struct {
	ID      string    `json:"id"`
	Label   string    `json:"label,omitempty"`
	Runtime string    `json:"runtime,omitempty"`
	SeenAt  time.Time `json:"seen_at"`
	ReadTo  int64     `json:"read_to"`
	Unread  int       `json:"unread"`
	Posted  int       `json:"posted"`
	// Spend is present only when a usage actor has exactly this id. Transcript
	// actors are derived from file names unless the agent imported under its
	// own --as, so most sessions will show nothing here rather than a wrong
	// number attached to the wrong name.
	Spend *usage.Summary `json:"spend,omitempty"`
}

// Usage is the cost pane. Scope, Since and Run are echoed back because a total
// means nothing without knowing what it covers.
type Usage struct {
	Scope string `json:"scope"`
	Since string `json:"since,omitempty"`
	// Run is the run-id prefix the numbers are narrowed to, if any. It applies
	// to spend alone: events carry no run id, so the rest of the page keeps
	// showing the whole session and the UI has to say so.
	Run       string          `json:"run,omitempty"`
	Total     usage.Summary   `json:"total"`
	ByActor   []usage.Summary `json:"by_actor"`
	ByModel   []usage.Summary `json:"by_model"`
	ByScope   []usage.Summary `json:"by_scope"`
	ByProject []usage.Summary `json:"by_project"`
	ByRun     []usage.Summary `json:"by_run"`
	BySource  []usage.Summary `json:"by_source"`
	ByDay     []usage.Summary `json:"by_day"`
}

const (
	defaultTimeline = 60
	maxTimeline     = 500
)

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	// Bad input is answered with 400 and the reason. A 500 here would send
	// someone reading server logs for a mistake that is in the query string.
	if t := q.Get("type"); t != "" && !event.Type(t).Valid() {
		http.Error(w, fmt.Sprintf("unknown event type %q", t), http.StatusBadRequest)
		return
	}
	if _, err := usage.ParseSince(q.Get("since")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	st, err := s.state(ctx, params{
		Type:        q.Get("type"),
		Since:       q.Get("since"),
		Run:         q.Get("run"),
		Limit:       clampLimit(q.Get("limit")),
		AllProjects: q.Get("all_projects") == "1",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The page polls; a cached snapshot is a stale snapshot.
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		return // the client went away mid-write; nothing useful to say
	}
}

// params is one page's worth of query string, kept as a struct so adding a
// filter does not mean threading another positional argument through.
type params struct {
	Type        string
	Since       string
	Run         string
	Limit       int
	AllProjects bool
}

func (s *Server) state(ctx context.Context, p params) (*State, error) {
	typ := p.Type
	if typ != "" && !event.Type(typ).Valid() {
		return nil, fmt.Errorf("unknown event type %q", typ)
	}
	sess := s.cfg.Session
	out := &State{
		Session:     sess,
		Workspace:   s.cfg.Workspace,
		GeneratedAt: time.Now(),
		Types:       typeNames(),
	}

	var err error
	if out.Head, err = s.st.MaxSeq(ctx, sess); err != nil {
		return nil, err
	}
	if out.Counts, err = s.st.CountsByType(ctx, sess); err != nil {
		return nil, err
	}
	if out.Questions, err = s.st.OpenQuestions(ctx, sess); err != nil {
		return nil, err
	}
	// Newest first: an open question from an hour ago is more actionable than
	// one from the first minute of the session.
	slices.Reverse(out.Questions)

	if out.Decisions, err = s.st.Latest(ctx, sess, string(event.Decision), 50); err != nil {
		return nil, err
	}
	if out.Timeline, err = s.st.Latest(ctx, sess, typ, p.Limit); err != nil {
		return nil, err
	}

	u, err := s.usage(ctx, p)
	if err != nil {
		return nil, err
	}
	out.Usage = *u

	spend := map[string]usage.Summary{}
	for _, a := range u.ByActor {
		spend[a.Key] = a
	}
	ps, err := s.st.Participants(ctx, sess)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		v := Participant{ID: p.ID, Label: p.Label, Runtime: p.Runtime, SeenAt: p.SeenAt}
		if v.ReadTo, err = s.st.Watermark(ctx, sess, p.ID); err != nil {
			return nil, err
		}
		if v.Unread, err = s.st.UnreadCount(ctx, sess, p.ID); err != nil {
			return nil, err
		}
		if v.Posted, err = s.st.PostedCount(ctx, sess, p.ID); err != nil {
			return nil, err
		}
		if sp, ok := spend[p.ID]; ok {
			v.Spend = &sp
		}
		out.Participants = append(out.Participants, v)
	}
	return out, nil
}

func (s *Server) usage(ctx context.Context, p params) (*Usage, error) {
	f := store.Filter{RunPrefix: p.Run}
	if !p.AllProjects && s.cfg.Workspace != "" {
		f.CWDPrefix = s.cfg.Workspace
	}
	t, err := usage.ParseSince(p.Since)
	if err != nil {
		return nil, err
	}
	f.Since = t

	out := &Usage{Scope: "all projects", Since: p.Since, Run: p.Run}
	if f.CWDPrefix != "" {
		out.Scope = f.CWDPrefix
	}
	if out.Total, err = s.st.UsageTotal(ctx, s.cfg.Session, f); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		name string
		dst  *[]usage.Summary
	}{
		{"actor", &out.ByActor},
		{"model", &out.ByModel},
		{"scope", &out.ByScope},
		{"project", &out.ByProject},
		{"source", &out.BySource},
		{"day", &out.ByDay},
	} {
		rows, err := s.st.UsageBy(ctx, s.cfg.Session, d.name, f)
		if err != nil {
			return nil, err
		}
		*d.dst = rows
	}

	// The run list is navigation, not an aggregate: it is computed without the
	// run filter so that picking a run does not collapse the list to the one
	// run you picked, leaving no way back.
	unfiltered := f
	unfiltered.RunPrefix = ""
	if out.ByRun, err = s.st.UsageBy(ctx, s.cfg.Session, "run", unfiltered); err != nil {
		return nil, err
	}
	// Every other dimension is ranked by cost, which is what you want when
	// asking "where did it go". A day axis ranked by cost is a scrambled chart.
	slices.SortFunc(out.ByDay, func(a, b usage.Summary) int { return cmp.Compare(a.Key, b.Key) })
	return out, nil
}

func clampLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultTimeline
	}
	return min(n, maxTimeline)
}

func typeNames() []string {
	ts := event.AllTypes()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}
