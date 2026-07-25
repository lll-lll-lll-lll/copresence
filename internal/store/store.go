// Package store persists the session log in SQLite.
//
// There is no daemon. Every participant's MCP server process opens the same
// database file directly; SQLite in WAL mode handles the concurrency. This
// removes an entire class of problems (lifecycle, orphan processes, socket
// paths) at the cost of ruling out network sharing, which v0 does not want.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DirName is the per-workspace state directory, created at the workspace root.
const DirName = ".copresence"

type Store struct {
	db   *sql.DB
	Path string
}

// Open opens (and migrates) the database at path, creating parent dirs.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Written here rather than only in `init`, because the directory can also be
	// created by a bare `post` in a repo nobody initialized, and an untracked
	// .copresence/ showing up in git status is a bad first impression.
	if err := writeSelfIgnore(dir); err != nil {
		return nil, err
	}
	// busy_timeout matters more than usual here: several agent processes write
	// concurrently and we would rather block briefly than surface SQLITE_BUSY
	// to a model that has no idea what to do with it.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, Path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS events (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  session    TEXT    NOT NULL,
  ts         TEXT    NOT NULL,
  actor      TEXT    NOT NULL,
  type       TEXT    NOT NULL,
  subject    TEXT    NOT NULL DEFAULT '',
  body       TEXT    NOT NULL,
  refs       TEXT    NOT NULL DEFAULT '[]',
  tags       TEXT    NOT NULL DEFAULT '[]',
  supersedes INTEGER,
  est_tokens INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_session_seq  ON events(session, seq);
CREATE INDEX IF NOT EXISTS events_supersedes   ON events(supersedes) WHERE supersedes IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_session_type ON events(session, type);

CREATE TABLE IF NOT EXISTS participants (
  session   TEXT NOT NULL,
  id        TEXT NOT NULL,
  label     TEXT NOT NULL DEFAULT '',
  runtime   TEXT NOT NULL DEFAULT '',
  joined_at TEXT NOT NULL,
  seen_at   TEXT NOT NULL,
  PRIMARY KEY (session, id)
);

CREATE TABLE IF NOT EXISTS watermarks (
  session     TEXT    NOT NULL,
  participant TEXT    NOT NULL,
  last_seq    INTEGER NOT NULL,
  PRIMARY KEY (session, participant)
);

-- trigram, not unicode61: unicode61 does not segment CJK, so a Japanese body
-- becomes one enormous token and is effectively unsearchable. Since "folded
-- detail stays recoverable through search" is load-bearing, that is fatal.
-- The cost is that queries shorter than 3 characters cannot use the index;
-- Search falls back to LIKE for those.
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
  body, subject, content='events', content_rowid='seq', tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
  INSERT INTO events_fts(rowid, body, subject) VALUES (new.seq, new.body, new.subject);
END;
CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, body, subject) VALUES ('delete', old.seq, old.body, old.subject);
END;
-- Events are immutable, so this trigger should never fire. It exists so that
-- the external-content index cannot silently desynchronize if that ever stops
-- being true (compaction, for instance).
CREATE TRIGGER IF NOT EXISTS events_au AFTER UPDATE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, body, subject) VALUES ('delete', old.seq, old.body, old.subject);
  INSERT INTO events_fts(rowid, body, subject) VALUES (new.seq, new.body, new.subject);
END;
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, usageSchema); err != nil {
		return err
	}
	if err := s.migrateUsage(ctx); err != nil {
		return err
	}
	return s.migrateTokenizer(ctx)
}

// migrateTokenizer rebuilds the FTS index if it was created with an older
// tokenizer. Without this, a database made before the trigram switch keeps its
// unicode61 index and stays silently unsearchable in Japanese.
func (s *Store) migrateTokenizer(ctx context.Context) error {
	var ddl string
	err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events_fts'`).Scan(&ddl)
	if err != nil || strings.Contains(ddl, "trigram") {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		DROP TABLE events_fts;
		CREATE VIRTUAL TABLE events_fts USING fts5(
		  body, subject, content='events', content_rowid='seq', tokenize='trigram');
		INSERT INTO events_fts(events_fts) VALUES ('rebuild');`)
	return err
}

func writeSelfIgnore(dir string) error {
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err == nil {
		return nil
	}
	return os.WriteFile(gi, []byte("*\n!.gitignore\n"), 0o644)
}

// FindRoot walks up from dir looking for an existing .copresence directory,
// falling back to the nearest git repository root.
func FindRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	var gitRoot string
	for d := abs; ; {
		if fi, err := os.Stat(filepath.Join(d, DirName)); err == nil && fi.IsDir() {
			return d, nil
		}
		if gitRoot == "" {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				gitRoot = d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if gitRoot != "" {
		return gitRoot, nil
	}
	return "", errors.New("no .copresence or .git found; run `copresence init` first")
}

// DBPath returns the database path for a workspace root.
func DBPath(root string) string { return filepath.Join(root, DirName, "session.db") }
