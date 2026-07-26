package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lll-lll-lll-lll/copresence/internal/usage"
)

const usageSchema = `
CREATE TABLE IF NOT EXISTS usage (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session      TEXT    NOT NULL,
  actor        TEXT    NOT NULL,
  ts           TEXT    NOT NULL,
  source       TEXT    NOT NULL,
  external_id  TEXT    NOT NULL,
  model        TEXT    NOT NULL,
  speed        TEXT    NOT NULL DEFAULT '',
  cwd          TEXT    NOT NULL DEFAULT '',
  -- The runtime's own session id, not the copresence session above.
  run_id       TEXT    NOT NULL DEFAULT '',
  subagent     INTEGER NOT NULL DEFAULT 0,
  input_tokens        INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_write_5m      INTEGER NOT NULL DEFAULT 0,
  cache_write_1h      INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL    NOT NULL DEFAULT 0,
  priced       INTEGER NOT NULL DEFAULT 1
);
-- The uniqueness constraint is what makes import idempotent: transcripts grow,
-- so the same file is imported repeatedly and must not double-count.
CREATE UNIQUE INDEX IF NOT EXISTS usage_external ON usage(source, external_id);
CREATE INDEX IF NOT EXISTS usage_session_ts ON usage(session, ts);
`

// migrateUsage adds columns to a usage table created by an earlier version.
// SQLite has no ADD COLUMN IF NOT EXISTS, so the current shape is read first.
func (s *Store) migrateUsage(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('usage')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for col, ddl := range map[string]string{
		"cwd":    `ALTER TABLE usage ADD COLUMN cwd TEXT NOT NULL DEFAULT ''`,
		"run_id": `ALTER TABLE usage ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
	} {
		if have[col] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	// Earlier versions imported the client's fabricated assistant turns as if
	// they were model calls. They are all-zero rows for a model that has no
	// price and never will, so they inflate the call count and produce an
	// unpriced-model warning forever. Removing them is safe precisely because
	// they carry no tokens and no cost: nothing about a total changes.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM usage WHERE model = '<synthetic>'`); err != nil {
		return err
	}

	// Indexes on migrated columns belong here, not in the schema constant: that
	// runs before the ALTER, so an index over cwd would fail on any database
	// created by an earlier version.
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS usage_cwd ON usage(session, cwd)`,
		`CREATE INDEX IF NOT EXISTS usage_run ON usage(session, run_id)`,
	} {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// UsageWrite reports what an import actually did. Inserted and Backfilled are
// separated because conflating them makes a re-import look like it discovered
// hundreds of new calls when it only filled in a column.
type UsageWrite struct {
	Inserted   int
	Backfilled int
}

// RecordUsage inserts usage rows, skipping any already imported.
func (s *Store) RecordUsage(ctx context.Context, session string, recs []usage.Record) (UsageWrite, error) {
	if session == "" {
		session = "main"
	}
	var w UsageWrite
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return w, err
	}
	defer tx.Rollback()

	var before int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage WHERE session = ?`, session).Scan(&before); err != nil {
		return w, err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage (session, actor, ts, source, external_id, model, speed, cwd, run_id, subagent,
		                   input_tokens, output_tokens, cache_read_tokens, cache_write_5m,
		                   cache_write_1h, cost_usd, priced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		-- Backfill only. A re-import must not restate an existing row's cost:
		-- prices are resolved once, at import, so history cannot be rewritten by
		-- a later rate change. An empty cwd or run_id is a gap left by an older
		-- schema, not a priced decision, so filling it in is safe. A column that
		-- already has a value is never overwritten, and the WHERE clause keeps
		-- rows with nothing to fill out of the affected count.
		ON CONFLICT(source, external_id) DO UPDATE SET
		    cwd    = CASE WHEN usage.cwd    = '' THEN excluded.cwd    ELSE usage.cwd    END,
		    run_id = CASE WHEN usage.run_id = '' THEN excluded.run_id ELSE usage.run_id END
		  WHERE usage.cwd = '' OR usage.run_id = ''`)
	if err != nil {
		return w, err
	}
	defer stmt.Close()

	touched := 0
	for _, r := range recs {
		res, err := stmt.ExecContext(ctx, session, r.Actor, r.TS.UTC().Format(time.RFC3339Nano),
			r.Source, r.ExternalID, r.Model, r.Speed, r.CWD, r.RunID, boolInt(r.Subagent),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWrite5m,
			r.CacheWrite1h, r.CostUSD, boolInt(r.Priced))
		if err != nil {
			return w, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			touched++
		}
	}
	var after int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage WHERE session = ?`, session).Scan(&after); err != nil {
		return w, err
	}
	w.Inserted = after - before
	w.Backfilled = touched - w.Inserted
	return w, tx.Commit()
}

// Filter narrows a usage query. An empty CWDPrefix means every project.
type Filter struct {
	Since time.Time
	// CWDPrefix scopes to work done at or below a directory. Transcripts are
	// keyed by the directory a session started in, so a session opened in a
	// parent directory carries sibling projects' spend with it — on the log this
	// was built against, 13% of the reported total belonged to other projects.
	CWDPrefix string
	// RunPrefix narrows to one run of an agent runtime. It matches on a prefix
	// because run ids are UUIDs and nobody types those: `--run 4afcba68` is how
	// the question actually gets asked. An ambiguous prefix matches several
	// runs, which is visible in the output rather than silent.
	RunPrefix string
}

func (f Filter) apply(sb *strings.Builder, args *[]any) {
	if !f.Since.IsZero() {
		sb.WriteString(` AND ts >= ?`)
		*args = append(*args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if f.RunPrefix != "" {
		// No escape for the empty run id here: a row with no run recorded is
		// not a member of any run, and including it would attach unattributed
		// spend to whichever run happened to be asked about.
		sb.WriteString(` AND run_id LIKE ? ESCAPE '\'`)
		*args = append(*args, escapeLike(f.RunPrefix)+"%")
	}
	if f.CWDPrefix != "" {
		// Rows imported before cwd was recorded have none; excluding them would
		// silently shrink historical totals, so they stay in and are reported
		// under "(unknown)".
		sb.WriteString(` AND (cwd = '' OR cwd = ? OR cwd LIKE ? ESCAPE '\')`)
		*args = append(*args, f.CWDPrefix, escapeLike(f.CWDPrefix)+"/%")
	}
}

// UsageBy aggregates spend along one dimension: "actor", "model", "day",
// "scope" (main loop vs subagent), "project" (the directory worked in), "run"
// (one invocation of an agent runtime), or "source" (which runtime).
func (s *Store) UsageBy(ctx context.Context, session, dimension string, f Filter) ([]usage.Summary, error) {
	var key string
	switch dimension {
	case "actor":
		key = "actor"
	case "model":
		key = "model"
	case "day":
		key = "substr(ts, 1, 10)"
	case "scope":
		key = `CASE subagent WHEN 1 THEN 'subagent' ELSE 'main' END`
	case "project":
		key = `CASE cwd WHEN '' THEN '(unknown)' ELSE cwd END`
	case "run":
		// Qualified by source so two runtimes cannot merge into one row if
		// their id schemes ever overlap. The separator is ':' rather than '/'
		// so display code does not mistake the key for a path and elide the
		// head of the id — which is the part anyone reads.
		key = `CASE run_id WHEN '' THEN '(unknown)' ELSE source || ':' || run_id END`
	case "source":
		key = "source"
	default:
		return nil, fmt.Errorf("unknown dimension %q (want actor, model, day, scope, project, run, or source)", dimension)
	}

	var sb strings.Builder
	args := []any{session}
	fmt.Fprintf(&sb, `SELECT %s AS k, MIN(ts), MAX(ts), COUNT(*), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_5m + cache_write_1h), SUM(cost_usd),
		SUM(CASE priced WHEN 0 THEN 1 ELSE 0 END)
		FROM usage WHERE session = ?`, key)
	f.apply(&sb, &args)
	sb.WriteString(` GROUP BY k ORDER BY SUM(cost_usd) DESC`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usage.Summary
	for rows.Next() {
		var m usage.Summary
		var first, last string
		if err := rows.Scan(&m.Key, &first, &last, &m.Calls, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheWriteToken, &m.CostUSD, &m.UnpricedCalls); err != nil {
			return nil, err
		}
		m.First, _ = time.Parse(time.RFC3339Nano, first)
		m.Last, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, m)
	}
	return out, rows.Err()
}

// UsageTotal is the whole-session rollup.
func (s *Store) UsageTotal(ctx context.Context, session string, f Filter) (usage.Summary, error) {
	rows, err := s.UsageBy(ctx, session, "scope", f)
	if err != nil {
		return usage.Summary{}, err
	}
	total := usage.Summary{Key: "total"}
	for _, r := range rows {
		if total.First.IsZero() || r.First.Before(total.First) {
			total.First = r.First
		}
		if r.Last.After(total.Last) {
			total.Last = r.Last
		}
		total.Calls += r.Calls
		total.InputTokens += r.InputTokens
		total.OutputTokens += r.OutputTokens
		total.CacheReadTokens += r.CacheReadTokens
		total.CacheWriteToken += r.CacheWriteToken
		total.CostUSD += r.CostUSD
		total.UnpricedCalls += r.UnpricedCalls
	}
	return total, nil
}

// UsageRecords returns raw rows, newest first — the feed a dashboard reads.
func (s *Store) UsageRecords(ctx context.Context, session string, f Filter, limit int) ([]usage.Record, error) {
	if limit <= 0 {
		limit = 1000
	}
	var sb strings.Builder
	sb.WriteString(`SELECT id, session, actor, ts, source, external_id, model, speed, cwd, run_id, subagent,
		     input_tokens, output_tokens, cache_read_tokens, cache_write_5m, cache_write_1h,
		     cost_usd, priced
		  FROM usage WHERE session = ?`)
	args := []any{session}
	f.apply(&sb, &args)
	sb.WriteString(` ORDER BY ts DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usage.Record
	for rows.Next() {
		var r usage.Record
		var ts string
		var sub, priced int
		if err := rows.Scan(&r.ID, &r.Session, &r.Actor, &ts, &r.Source, &r.ExternalID,
			&r.Model, &r.Speed, &r.CWD, &r.RunID, &sub, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
			&r.CacheWrite5m, &r.CacheWrite1h, &r.CostUSD, &priced); err != nil {
			return nil, err
		}
		r.TS, _ = time.Parse(time.RFC3339Nano, ts)
		r.Subagent, r.Priced = sub == 1, priced == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
