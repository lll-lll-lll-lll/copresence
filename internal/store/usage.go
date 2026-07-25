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
	if !have["cwd"] {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE usage ADD COLUMN cwd TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// Indexes on migrated columns belong here, not in the schema constant: that
	// runs before the ALTER, so an index over cwd would fail on any database
	// created by an earlier version.
	_, err = s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS usage_cwd ON usage(session, cwd)`)
	return err
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
		INSERT INTO usage (session, actor, ts, source, external_id, model, speed, cwd, subagent,
		                   input_tokens, output_tokens, cache_read_tokens, cache_write_5m,
		                   cache_write_1h, cost_usd, priced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		-- Backfill only. A re-import must not restate an existing row's cost:
		-- prices are resolved once, at import, so history cannot be rewritten by
		-- a later rate change. An empty cwd is a gap from an older schema, not a
		-- priced decision, so filling it in is safe.
		ON CONFLICT(source, external_id) DO UPDATE SET cwd = excluded.cwd
		  WHERE usage.cwd = ''`)
	if err != nil {
		return w, err
	}
	defer stmt.Close()

	touched := 0
	for _, r := range recs {
		res, err := stmt.ExecContext(ctx, session, r.Actor, r.TS.UTC().Format(time.RFC3339Nano),
			r.Source, r.ExternalID, r.Model, r.Speed, r.CWD, boolInt(r.Subagent),
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
}

func (f Filter) apply(sb *strings.Builder, args *[]any) {
	if !f.Since.IsZero() {
		sb.WriteString(` AND ts >= ?`)
		*args = append(*args, f.Since.UTC().Format(time.RFC3339Nano))
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
// "scope" (main loop vs subagent), or "project" (the directory worked in).
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
	default:
		return nil, fmt.Errorf("unknown dimension %q (want actor, model, day, scope, or project)", dimension)
	}

	var sb strings.Builder
	args := []any{session}
	fmt.Fprintf(&sb, `SELECT %s AS k, COUNT(*), SUM(input_tokens), SUM(output_tokens),
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
		if err := rows.Scan(&m.Key, &m.Calls, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheWriteToken, &m.CostUSD, &m.UnpricedCalls); err != nil {
			return nil, err
		}
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
	sb.WriteString(`SELECT id, session, actor, ts, source, external_id, model, speed, cwd, subagent,
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
			&r.Model, &r.Speed, &r.CWD, &sub, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
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
