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

// RecordUsage inserts usage rows, skipping any already imported. It returns how
// many were new.
func (s *Store) RecordUsage(ctx context.Context, session string, recs []usage.Record) (int, error) {
	if session == "" {
		session = "main"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage (session, actor, ts, source, external_id, model, speed, subagent,
		                   input_tokens, output_tokens, cache_read_tokens, cache_write_5m,
		                   cache_write_1h, cost_usd, priced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, external_id) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	added := 0
	for _, r := range recs {
		res, err := stmt.ExecContext(ctx, session, r.Actor, r.TS.UTC().Format(time.RFC3339Nano),
			r.Source, r.ExternalID, r.Model, r.Speed, boolInt(r.Subagent),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWrite5m,
			r.CacheWrite1h, r.CostUSD, boolInt(r.Priced))
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// UsageBy aggregates spend along one dimension: "actor", "model", "day", or
// "scope" (main loop vs subagent).
func (s *Store) UsageBy(ctx context.Context, session, dimension string, since time.Time) ([]usage.Summary, error) {
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
	default:
		return nil, fmt.Errorf("unknown dimension %q (want actor, model, day, or scope)", dimension)
	}

	var sb strings.Builder
	args := []any{session}
	fmt.Fprintf(&sb, `SELECT %s AS k, COUNT(*), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_5m + cache_write_1h), SUM(cost_usd),
		SUM(CASE priced WHEN 0 THEN 1 ELSE 0 END)
		FROM usage WHERE session = ?`, key)
	if !since.IsZero() {
		sb.WriteString(` AND ts >= ?`)
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&sb, ` GROUP BY k ORDER BY SUM(cost_usd) DESC`)

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
func (s *Store) UsageTotal(ctx context.Context, session string, since time.Time) (usage.Summary, error) {
	rows, err := s.UsageBy(ctx, session, "scope", since)
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
func (s *Store) UsageRecords(ctx context.Context, session string, since time.Time, limit int) ([]usage.Record, error) {
	if limit <= 0 {
		limit = 1000
	}
	q := `SELECT id, session, actor, ts, source, external_id, model, speed, subagent,
		     input_tokens, output_tokens, cache_read_tokens, cache_write_5m, cache_write_1h,
		     cost_usd, priced
		  FROM usage WHERE session = ?`
	args := []any{session}
	if !since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
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
			&r.Model, &r.Speed, &sub, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
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
