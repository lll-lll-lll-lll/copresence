package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/event"
)

// notSuperseded is the guard applied to every read path. A correction posted
// with supersedes=N retires event N everywhere at once, which is what keeps a
// wrong finding from haunting the session forever.
//
// The session predicate matters: without it, a correction posted in one session
// silently retires an unrelated event that happens to share a seq in another.
const notSuperseded = `NOT EXISTS (
	SELECT 1 FROM events x WHERE x.supersedes = e.seq AND x.session = e.session)`

const selectCols = `e.seq, e.session, e.ts, e.actor, e.type, e.subject, e.body, e.refs, e.tags, COALESCE(e.supersedes,0), e.est_tokens`

// Post appends an event and returns its sequence number.
func (s *Store) Post(ctx context.Context, e *event.Event) (int64, error) {
	if e.Session == "" {
		e.Session = "main"
	}
	e.Normalize()
	if err := e.Validate(); err != nil {
		return 0, err
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	// Refusing a dangling supersedes closes two holes at once: a typo that
	// retires nothing while looking like a correction, and a seq from another
	// session retiring a stranger's event.
	if e.Supersedes > 0 {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE seq = ? AND session = ?`,
			e.Supersedes, e.Session).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("supersedes: #%d does not exist in session %q", e.Supersedes, e.Session)
		}
	}
	if err := s.checkAnswerTargets(ctx, e); err != nil {
		return 0, err
	}
	e.EstTokens = e.Cost()

	refs, _ := json.Marshal(orEmpty(e.Refs))
	tags, _ := json.Marshal(orEmpty(e.Tags))
	var sup any
	if e.Supersedes > 0 {
		sup = e.Supersedes
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (session, ts, actor, type, subject, body, refs, tags, supersedes, est_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Session, e.TS.UTC().Format(time.RFC3339Nano), e.Actor, string(e.Type),
		e.Subject, e.Body, string(refs), string(tags), sup, e.EstTokens)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	e.Seq = seq
	return seq, nil
}

// checkAnswerTargets rejects an answer whose event: refs do not point at a live
// question in the same session. An answer that closes nothing is the worst
// outcome available: it reads as resolved to a human while the question stays
// open forever.
func (s *Store) checkAnswerTargets(ctx context.Context, e *event.Event) error {
	if e.Type != event.Answer {
		return nil
	}
	for _, target := range e.EventRefs() {
		var typ string
		err := s.db.QueryRowContext(ctx,
			`SELECT type FROM events WHERE seq = ? AND session = ?`, target, e.Session).Scan(&typ)
		if err == sql.ErrNoRows {
			return fmt.Errorf("answer refs event:%d, which does not exist in session %q", target, e.Session)
		}
		if err != nil {
			return err
		}
		if event.Type(typ) != event.Question {
			return fmt.Errorf("answer refs event:%d, which is a %s, not a question", target, typ)
		}
	}
	return nil
}

// Unread returns events the participant has not consumed yet, oldest first.
//
// upTo bounds the read to a sequence number the caller has already observed.
// This is what makes catchup safe against concurrent writers: without it, the
// caller reads unread events in one statement and the head in another, and
// anything inserted in between gets marked read without ever being delivered.
//
// A participant never receives its own events back: it already knows them, and
// echoing them wastes the context budget the assembler is trying to protect.
func (s *Store) Unread(ctx context.Context, session, participant string, upTo int64, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = DefaultUnreadLimit
	}
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ?
		   AND e.actor <> ?
		   AND e.seq <= ?
		   AND e.seq > COALESCE((SELECT last_seq FROM watermarks WHERE session = ? AND participant = ?), 0)
		   AND `+notSuperseded+`
		 ORDER BY e.seq LIMIT ?`,
		session, participant, upTo, session, participant, limit)
}

// DefaultUnreadLimit caps how many unread events one catchup will consider.
// Callers must treat a full result as truncated and advance the watermark only
// as far as the last row they actually received.
const DefaultUnreadLimit = 500

// OpenQuestions returns unanswered questions regardless of watermark. These are
// re-served on every catchup on purpose: an open question that scrolls past
// unread is a hole every other participant will independently rediscover.
func (s *Store) OpenQuestions(ctx context.Context, session string) ([]event.Event, error) {
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND e.type = 'question' AND `+notSuperseded+`
		   AND NOT EXISTS (
		     SELECT 1 FROM events a, json_each(a.refs) r
		     WHERE a.type = 'answer' AND a.session = e.session
		       AND r.value = 'event:' || e.seq
		       -- a retracted answer must reopen the question, or supersedes
		       -- would be the one guard that does not hold everywhere
		       AND NOT EXISTS (
		         SELECT 1 FROM events y WHERE y.supersedes = a.seq AND y.session = a.session)
		   )
		 ORDER BY e.seq`,
		session)
}

// LatestStatus returns the most recent status per participant, excluding self.
// This is the collision detector: it is how an agent notices someone else is
// already inside the file it was about to open.
func (s *Store) LatestStatus(ctx context.Context, session, excludeActor string) ([]event.Event, error) {
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND e.type = 'status' AND e.actor <> ? AND `+notSuperseded+`
		   AND e.seq = (SELECT MAX(seq) FROM events m WHERE m.session = e.session AND m.type = 'status' AND m.actor = e.actor)
		 ORDER BY e.seq DESC`,
		session, excludeActor)
}

// Search runs full-text search over body and subject.
//
// Queries arrive from a model, and the most natural thing for it to search for
// is a file path — which is also a string full of FTS5 operators. Every term is
// therefore quoted into a literal, and anything the index cannot serve falls
// back to LIKE. A search must return no rows, never an error.
func (s *Store) Search(ctx context.Context, session, q, typ, subject string, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 20
	}
	terms := strings.Fields(q)
	useLike := false
	for _, t := range terms {
		// trigram cannot index anything shorter than three characters.
		if len([]rune(t)) < 3 {
			useLike = true
		}
	}
	evs, err := s.search(ctx, session, terms, typ, subject, limit, useLike)
	if err != nil && !useLike {
		// An FTS5 parse error we failed to anticipate is still not the caller's
		// problem; degrade to substring matching rather than surfacing SQL.
		return s.search(ctx, session, terms, typ, subject, limit, true)
	}
	return evs, err
}

func (s *Store) search(ctx context.Context, session string, terms []string, typ, subject string, limit int, useLike bool) ([]event.Event, error) {
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT ` + selectCols + ` FROM events e `)
	fts := len(terms) > 0 && !useLike
	if fts {
		sb.WriteString(`JOIN events_fts f ON f.rowid = e.seq `)
	}
	sb.WriteString(`WHERE e.session = ? AND ` + notSuperseded)
	args = append(args, session)
	switch {
	case fts:
		sb.WriteString(` AND events_fts MATCH ?`)
		args = append(args, ftsLiteral(terms))
	case len(terms) > 0:
		for _, t := range terms {
			sb.WriteString(` AND (e.body LIKE ? ESCAPE '\' OR e.subject LIKE ? ESCAPE '\')`)
			pat := "%" + escapeLike(t) + "%"
			args = append(args, pat, pat)
		}
	}
	if typ != "" {
		sb.WriteString(` AND e.type = ?`)
		args = append(args, typ)
	}
	if subject != "" {
		sb.WriteString(` AND e.subject LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(subject)+"%")
	}
	if fts {
		sb.WriteString(` ORDER BY bm25(events_fts), e.seq DESC`)
	} else {
		sb.WriteString(` ORDER BY e.seq DESC`)
	}
	sb.WriteString(` LIMIT ?`)
	args = append(args, limit)
	return s.query(ctx, sb.String(), args...)
}

// ftsLiteral turns each term into a quoted FTS5 string so that punctuation is
// matched rather than parsed. Terms are space-joined, which FTS5 reads as AND.
func ftsLiteral(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ContextFor returns everything known about a subject: events whose subject is
// at or below it (path-prefix semantics), plus events that ref it as a file.
func (s *Store) ContextFor(ctx context.Context, session, subject string, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND `+notSuperseded+`
		   AND (e.subject = ? OR e.subject LIKE ? OR EXISTS (
		         SELECT 1 FROM json_each(e.refs) r WHERE r.value = 'file:' || ?
		   ))
		 ORDER BY e.seq DESC LIMIT ?`,
		session, subject, subject+"%", subject, limit)
}

// Since returns events after seq, oldest first. Used by the CLI and timeline.
func (s *Store) Since(ctx context.Context, session string, seq int64, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND e.seq > ? AND `+notSuperseded+`
		 ORDER BY e.seq LIMIT ?`,
		session, seq, limit)
}

// Latest returns live events newest first, optionally restricted to one type.
//
// Since reads forward from a seq, which is what a catching-up participant
// wants. A human opening a dashboard on a long-running session wants the other
// end: the last twenty events, not the first twenty.
func (s *Store) Latest(ctx context.Context, session, typ string, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + selectCols + ` FROM events e WHERE e.session = ? AND ` + notSuperseded
	args := []any{session}
	if typ != "" {
		q += ` AND e.type = ?`
		args = append(args, typ)
	}
	q += ` ORDER BY e.seq DESC LIMIT ?`
	args = append(args, limit)
	return s.query(ctx, q, args...)
}

// CountsByType reports how many live events of each type the session holds.
func (s *Store) CountsByType(ctx context.Context, session string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.type, COUNT(*) FROM events e WHERE e.session = ? AND `+notSuperseded+` GROUP BY e.type`,
		session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// ByType returns all live events of a type, oldest first.
func (s *Store) ByType(ctx context.Context, session string, t event.Type) ([]event.Event, error) {
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND e.type = ? AND `+notSuperseded+`
		 ORDER BY e.seq`,
		session, string(t))
}

// UnreadCount reports how many events are waiting for a participant. It backs
// the reciprocity hint on post: telling an agent what it stands to gain is the
// only lever that makes writing pay for the writer.
func (s *Store) UnreadCount(ctx context.Context, session, participant string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events e
		 WHERE e.session = ? AND e.actor <> ?
		   AND e.seq > COALESCE((SELECT last_seq FROM watermarks WHERE session = ? AND participant = ?), 0)
		   AND `+notSuperseded,
		session, participant, session, participant).Scan(&n)
	return n, err
}

// PostedCount reports how many events an actor has written.
func (s *Store) PostedCount(ctx context.Context, session, actor string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session = ? AND actor = ?`, session, actor).Scan(&n)
	return n, err
}

// Retired maps each superseded seq to the seq that retired it, so a human can
// answer "where did my event go?" instead of watching it vanish.
func (s *Store) Retired(ctx context.Context, session string) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT supersedes, seq FROM events WHERE session = ? AND supersedes IS NOT NULL`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var old, by int64
		if err := rows.Scan(&old, &by); err != nil {
			return nil, err
		}
		out[old] = by
	}
	return out, rows.Err()
}

// SinceAll is Since without the supersedes filter, for the human-facing log.
func (s *Store) SinceAll(ctx context.Context, session string, seq int64, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.query(ctx,
		`SELECT `+selectCols+` FROM events e
		 WHERE e.session = ? AND e.seq > ?
		 ORDER BY e.seq LIMIT ?`,
		session, seq, limit)
}

// MaxSeq returns the highest sequence number in the session (0 if empty).
func (s *Store) MaxSeq(ctx context.Context, session string) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session = ?`, session).Scan(&seq)
	return seq.Int64, err
}

// Watermark returns how far a participant has read.
func (s *Store) Watermark(ctx context.Context, session, participant string) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_seq FROM watermarks WHERE session = ? AND participant = ?`, session, participant).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq.Int64, err
}

// Advance moves a participant's watermark forward. It never moves backward, so
// a concurrent catchup cannot cause events to be served twice.
func (s *Store) Advance(ctx context.Context, session, participant string, seq int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO watermarks (session, participant, last_seq) VALUES (?, ?, ?)
		 ON CONFLICT(session, participant) DO UPDATE SET last_seq = MAX(last_seq, excluded.last_seq)`,
		session, participant, seq)
	return err
}

// Join registers a participant, refreshing seen_at on every call.
func (s *Store) Join(ctx context.Context, session, id, label, runtime string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO participants (session, id, label, runtime, joined_at, seen_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session, id) DO UPDATE SET seen_at = excluded.seen_at,
		   label = CASE WHEN excluded.label <> '' THEN excluded.label ELSE label END,
		   runtime = CASE WHEN excluded.runtime <> '' THEN excluded.runtime ELSE runtime END`,
		session, id, label, runtime, now, now)
	return err
}

type Participant struct {
	ID, Label, Runtime string
	JoinedAt, SeenAt   time.Time
}

func (s *Store) Participants(ctx context.Context, session string) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, runtime, joined_at, seen_at FROM participants WHERE session = ? ORDER BY joined_at`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		var joined, seen string
		if err := rows.Scan(&p.ID, &p.Label, &p.Runtime, &joined, &seen); err != nil {
			return nil, err
		}
		p.JoinedAt, _ = time.Parse(time.RFC3339Nano, joined)
		p.SeenAt, _ = time.Parse(time.RFC3339Nano, seen)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Sessions lists session names with event counts.
func (s *Store) Sessions(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, COUNT(*) FROM events GROUP BY session ORDER BY session`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]event.Event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var (
			e          event.Event
			ts         string
			refs, tags string
			typ        string
		)
		if err := rows.Scan(&e.Seq, &e.Session, &ts, &e.Actor, &typ, &e.Subject, &e.Body,
			&refs, &tags, &e.Supersedes, &e.EstTokens); err != nil {
			return nil, err
		}
		e.Type = event.Type(typ)
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		_ = json.Unmarshal([]byte(refs), &e.Refs)
		_ = json.Unmarshal([]byte(tags), &e.Tags)
		out = append(out, e)
	}
	return out, rows.Err()
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
