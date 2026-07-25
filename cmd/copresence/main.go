// Command copresence runs a shared session log for AI agents working in the
// same workspace.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/assemble"
	"github.com/lll-lll-lll-lll/copresence/internal/event"
	"github.com/lll-lll-lll-lll/copresence/internal/mcpserver"
	"github.com/lll-lll-lll-lll/copresence/internal/store"
)

const helpText = `copresence — a shared session log for AI agents in one workspace

usage: copresence <command> [flags]

  init                 create .copresence/ in the current directory
  mcp    [--as ID]     serve this session over MCP on stdio
  catchup --as ID      print the projection a participant would receive
  post    --as ID      append an event from the shell
  log                  print the timeline (--all to include retired events)
  search  QUERY        full-text search the log
  context SUBJECT      everything known about a file, prefix, or symbol
  digest               print decisions, open questions, participants
  export               write a committable Markdown summary
  usage                token and cost accounting (see: usage --help)
  doctor               report on the current workspace

Common flags: --session NAME (default "main"), --dir PATH (default: cwd)
Env: COPRESENCE_ACTOR, COPRESENCE_SESSION
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "copresence: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(helpText)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cmdInit(rest)
	case "mcp":
		return cmdMCP(rest)
	case "catchup":
		return cmdCatchup(rest)
	case "post":
		return cmdPost(rest)
	case "log":
		return cmdLog(rest)
	case "search":
		return cmdSearch(rest)
	case "context":
		return cmdContext(rest)
	case "digest":
		return cmdDigest(rest)
	case "export":
		return cmdExport(rest)
	case "usage":
		return cmdUsage(rest)
	case "doctor":
		return cmdDoctor(rest)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, helpText)
	}
}

// common holds the flags every command shares.
type common struct {
	dir     string
	session string
	actor   string
}

func bind(fs *flag.FlagSet, needActor bool) *common {
	c := &common{}
	fs.StringVar(&c.dir, "dir", ".", "workspace directory")
	fs.StringVar(&c.session, "session", envOr("COPRESENCE_SESSION", "main"), "session name")
	if needActor {
		fs.StringVar(&c.actor, "as", os.Getenv("COPRESENCE_ACTOR"), "participant id (required)")
	}
	return c
}

func (c *common) open() (*store.Store, error) {
	root, err := store.FindRoot(c.dir)
	if err != nil {
		return nil, err
	}
	return store.Open(store.DBPath(root))
}

// findRoot resolves the workspace root for a --dir value.
func findRoot(dir string) (string, error) { return store.FindRoot(dir) }

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	c := bind(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(c.dir)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, store.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A self-ignoring directory keeps session state out of git without editing
	// the repository's own .gitignore.
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		if err := os.WriteFile(gi, []byte("*\n!.gitignore\n"), 0o644); err != nil {
			return err
		}
	}
	st, err := store.Open(store.DBPath(root))
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("initialized %s\n\nRegister with an MCP client, e.g. Claude Code:\n  claude mcp add copresence -- copresence mcp --as claude-1 --dir %s\n", dir, root)
	return nil
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	c := bind(fs, true)
	runtime := fs.String("runtime", "", "runtime name, e.g. claude-code")
	label := fs.String("label", "", "human-readable label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A stable id preserves your read position across restarts, but sharing one
	// between two live agents is worse than having none: each would filter the
	// other's events out as "their own". Auto-generate rather than collide.
	if c.actor == "" {
		c.actor = autoActor(*runtime)
		fmt.Fprintf(os.Stderr, "copresence: no --as given, joining as %q (pass --as for a read position that survives restarts)\n", c.actor)
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := mcpserver.New(st, mcpserver.Config{
		Session: c.session, Actor: c.actor, Label: *label, Runtime: *runtime,
	})
	return srv.Run(ctx)
}

func cmdCatchup(args []string) error {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	c := bind(fs, true)
	budget := fs.Int("budget", assemble.DefaultBudget, "token budget")
	focus := fs.String("focus", "", "bias selection toward this path or keyword")
	peek := fs.Bool("peek", false, "do not advance the watermark")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.actor == "" {
		return fmt.Errorf("--as is required")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	res, err := assemble.Build(ctx, st, assemble.Request{
		Session: c.session, Participant: c.actor, Budget: *budget, Focus: *focus, Now: time.Now(),
	})
	if err != nil {
		return err
	}
	if !*peek {
		if err := st.Advance(ctx, c.session, c.actor, res.AsOf); err != nil {
			return err
		}
	}
	fmt.Println(res.Text)
	fmt.Fprintf(os.Stderr, "\n(%d shown, %d folded, ~%d tokens)\n", res.Included, res.Dropped, res.Tokens)
	return nil
}

func cmdPost(args []string) error {
	fs := flag.NewFlagSet("post", flag.ExitOnError)
	c := bind(fs, true)
	typ := fs.String("type", "note", "event type")
	subject := fs.String("subject", "", "where this applies")
	supersedes := fs.Int64("supersedes", 0, "seq this corrects")
	var refs, tags multiFlag
	fs.Var(&refs, "ref", "kind:value pointer (repeatable)")
	fs.Var(&tags, "tag", "tag (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.actor == "" {
		return fmt.Errorf("--as is required")
	}
	// Go's flag package stops parsing at the first positional argument, so a
	// trailing --flag would be silently swallowed into the body. Refuse instead.
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			return fmt.Errorf("flag %q appears after the body; put all flags before it", a)
		}
	}
	body := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if body == "" {
		return fmt.Errorf("body is required: copresence post --as me --type finding \"...\"")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	e := &event.Event{
		Session: c.session, Actor: c.actor, Type: event.Type(*typ),
		Subject: *subject, Body: body, Refs: refs, Tags: tags, Supersedes: *supersedes,
	}
	seq, err := st.Post(context.Background(), e)
	if err != nil {
		return err
	}
	_ = st.Join(context.Background(), c.session, c.actor, "", "cli")
	fmt.Printf("#%d\n", seq)
	return nil
}

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	c := bind(fs, false)
	since := fs.Int64("since", 0, "only events after this seq")
	limit := fs.Int("n", 50, "max events")
	all := fs.Bool("all", false, "include retired (superseded) events, marked with the seq that retired them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	var evs []event.Event
	if *all {
		evs, err = st.SinceAll(ctx, c.session, *since, *limit)
	} else {
		evs, err = st.Since(ctx, c.session, *since, *limit)
	}
	if err != nil {
		return err
	}
	// Without this a human watching their own event disappear has no way to
	// learn that it was retired rather than lost.
	retired, err := st.Retired(ctx, c.session)
	if err != nil {
		return err
	}
	for _, e := range evs {
		mark := ""
		if by, ok := retired[e.Seq]; ok {
			mark = fmt.Sprintf("  (retired by #%d)", by)
		}
		subj := ""
		if e.Subject != "" {
			subj = " @ " + e.Subject
		}
		fmt.Printf("#%-4d %-8s %-12s%s%s\n      %s\n", e.Seq, e.Type, e.Actor, subj, mark, oneLine(e.Body))
	}
	if len(evs) == 0 {
		fmt.Println("(empty)")
	}
	return nil
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	c := bind(fs, false)
	typ := fs.String("type", "", "restrict to an event type")
	subject := fs.String("subject", "", "restrict to a subject prefix")
	limit := fs.Int("n", 20, "max results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	evs, err := st.Search(context.Background(), c.session, strings.Join(fs.Args(), " "), *typ, *subject, *limit)
	if err != nil {
		return err
	}
	return printEvents(evs)
}

func cmdContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	c := bind(fs, false)
	limit := fs.Int("n", 50, "max results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	subject := strings.Join(fs.Args(), " ")
	if subject == "" {
		return fmt.Errorf("a subject is required: copresence context src/auth")
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	evs, err := st.ContextFor(context.Background(), c.session, subject, *limit)
	if err != nil {
		return err
	}
	return printEvents(evs)
}

func printEvents(evs []event.Event) error {
	if len(evs) == 0 {
		fmt.Println("(no matches)")
		return nil
	}
	now := time.Now()
	for _, e := range evs {
		subj := ""
		if e.Subject != "" {
			subj = " @ " + e.Subject
		}
		fmt.Printf("#%-4d %-8s %-12s%s  %s\n      %s\n",
			e.Seq, e.Type, e.Actor, subj, assemble.Ago(now, e.TS), oneLine(e.Body))
	}
	return nil
}

func cmdDigest(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	c := bind(fs, false)
	budget := fs.Int("budget", assemble.DefaultBudget, "token budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	out, err := assemble.Digest(context.Background(), st, c.session, *budget, time.Now())
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	c := bind(fs, false)
	out := fs.String("out", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	var b strings.Builder
	fmt.Fprintf(&b, "# Session: %s\n\n_Exported %s_\n", c.session, time.Now().Format(time.RFC3339))
	for _, sec := range []struct {
		title string
		t     event.Type
	}{
		{"Decisions", event.Decision},
		{"Findings", event.Finding},
		{"Artifacts", event.Artifact},
	} {
		evs, err := st.ByType(ctx, c.session, sec.t)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "\n## %s\n\n", sec.title)
		if len(evs) == 0 {
			b.WriteString("_none_\n")
		}
		for _, e := range evs {
			subj := ""
			if e.Subject != "" {
				subj = " — `" + e.Subject + "`"
			}
			fmt.Fprintf(&b, "- **#%d**%s (%s, %s)  \n  %s\n", e.Seq, subj, e.Actor,
				e.TS.Format("2006-01-02"), strings.ReplaceAll(strings.TrimSpace(e.Body), "\n", "\n  "))
		}
	}
	qs, err := st.OpenQuestions(ctx, c.session)
	if err != nil {
		return err
	}
	b.WriteString("\n## Open questions\n\n")
	if len(qs) == 0 {
		b.WriteString("_none_\n")
	}
	for _, q := range qs {
		fmt.Fprintf(&b, "- **#%d** (%s) %s\n", q.Seq, q.Actor, oneLine(q.Body))
	}

	if *out == "" {
		fmt.Print(b.String())
		return nil
	}
	return os.WriteFile(*out, []byte(b.String()), 0o644)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	c := bind(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := store.FindRoot(c.dir)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	fmt.Printf("workspace: %s\ndatabase:  %s\n", root, st.Path)
	sessions, err := st.Sessions(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("sessions:  %d\n", len(sessions))
	for name, n := range sessions {
		fmt.Printf("  %-16s %d events\n", name, n)
	}
	ps, err := st.Participants(ctx, c.session)
	if err != nil {
		return err
	}
	fmt.Printf("participants in %q: %d\n", c.session, len(ps))
	for _, p := range ps {
		wm, _ := st.Watermark(ctx, c.session, p.ID)
		fmt.Printf("  %-16s runtime=%-12s read-to=#%-4d last-seen=%s\n",
			p.ID, orDash(p.Runtime), wm, p.SeenAt.Local().Format("01-02 15:04"))
	}
	max, _ := st.MaxSeq(ctx, c.session)
	fmt.Printf("head: #%d\n", max)
	return nil
}

// autoActor builds a participant id that is unique among concurrently running
// processes on this machine.
func autoActor(runtime string) string {
	base := runtime
	if base == "" {
		base = "agent"
	}
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", base, os.Getpid())
	}
	return fmt.Sprintf("%s-%s", base, hex.EncodeToString(b[:]))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
