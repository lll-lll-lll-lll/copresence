package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SourceClaudeCode identifies records imported from a Claude Code transcript.
const SourceClaudeCode = "claude-code"

// claudeLine is the subset of a Claude Code JSONL entry that carries usage.
type claudeLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	CWD         string `json:"cwd"`
	Message     struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			Speed string `json:"speed"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseClaudeCode reads a Claude Code transcript and returns one record per
// billable model call.
//
// The transcript writes the same assistant message on several lines as it is
// revised, each carrying the identical final usage block. Deduplicating by
// message.id is therefore mandatory: on a real transcript, counting lines
// instead of messages overstated output tokens by 2.2x.
func ParseClaudeCode(r io.Reader, actor string) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // transcript lines can be very large

	var out []Record
	seen := map[string]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l claudeLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue // a malformed line is not a reason to lose the rest of the file
		}
		if l.Type != "assistant" || l.Message.Usage == nil || l.Message.ID == "" {
			continue
		}
		if seen[l.Message.ID] {
			continue
		}
		seen[l.Message.ID] = true

		ts, err := time.Parse(time.RFC3339, l.Timestamp)
		if err != nil {
			ts = time.Now()
		}
		u := l.Message.Usage
		rec := Record{
			Actor:           actor,
			TS:              ts,
			Source:          SourceClaudeCode,
			ExternalID:      l.Message.ID,
			Model:           l.Message.Model,
			Speed:           u.Speed,
			CWD:             l.CWD,
			Subagent:        l.IsSidechain,
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			CacheReadTokens: u.CacheReadInputTokens,
		}
		// The 5m/1h split matters: a 1h cache write costs 2x input where a 5m
		// write costs 1.25x. Fall back to the undifferentiated total as 5m only
		// when the breakdown is absent.
		if cc := u.CacheCreation; cc != nil && (cc.Ephemeral5m > 0 || cc.Ephemeral1h > 0) {
			rec.CacheWrite5m = cc.Ephemeral5m
			rec.CacheWrite1h = cc.Ephemeral1h
		} else {
			rec.CacheWrite5m = u.CacheCreationInputTokens
		}
		rec.Cost()
		out = append(out, rec)
	}
	return out, sc.Err()
}

// SubagentDir is the directory Claude Code nests delegated agents' transcripts
// under, below the parent session's own directory.
const SubagentDir = "subagents"

// subagentActor extracts the delegated agent's id from its transcript path, or
// returns "" for a main-loop transcript.
//
// Delegated work is the one unit of spend that is exactly attributable: one
// subagent transcript is one delegated task, start to finish. Folding it into
// the parent's actor throws that away.
func subagentActor(path string) string {
	if filepath.Base(filepath.Dir(path)) != SubagentDir {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// ParseClaudeCodeFile is ParseClaudeCode over a path.
//
// A delegated agent keeps its own identity even when the caller passes an
// actor: charging a subagent's tokens to whoever ran the import would make the
// parent look expensive and the delegation free.
func ParseClaudeCodeFile(path, actor string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	delegated := subagentActor(path)
	switch {
	case delegated != "":
		actor = delegated
	case actor == "":
		actor = "claude-" + shortID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	}

	recs, err := ParseClaudeCode(f, actor)
	if err != nil || delegated == "" {
		return recs, err
	}
	// Lines inside a subagent transcript do not carry isSidechain; the path is
	// what marks them as delegated.
	for i := range recs {
		recs[i].Subagent = true
	}
	return recs, nil
}

// DiscoverClaudeCode returns the transcripts Claude Code has written for a
// workspace, along with the directory they came from.
//
// Claude Code keys transcripts by the directory the session was started in, not
// by repository root — a session opened in a parent directory files its
// transcript under the parent. So this walks up from the workspace and returns
// the nearest ancestor that has any. The caller should report which directory
// was used: an ancestor's transcripts cover sibling projects too.
func DiscoverClaudeCode(workspace string) (paths []string, from string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, "", err
	}
	var tried []string
	for d := abs; ; {
		dir := filepath.Join(home, ".claude", "projects", slugify(d))
		matches, err := transcriptsUnder(dir)
		if err != nil {
			return nil, "", err
		}
		if len(matches) > 0 {
			return matches, d, nil
		}
		tried = append(tried, d)
		parent := filepath.Dir(d)
		if parent == d || d == home {
			break
		}
		d = parent
	}
	return nil, "", fmt.Errorf("no Claude Code transcripts for %s or its parents (checked %d directories); pass paths explicitly",
		abs, len(tried))
}

// transcriptsUnder returns every transcript in a project directory, including
// the ones Claude Code nests under <session-id>/subagents/.
//
// Globbing only the top level silently drops all delegated work. On the session
// this was written in, that hid 39 model calls and $2.28 — the parent sits idle
// while a subagent runs, so the loss shows up as a plausible-looking gap in the
// timeline rather than as an obvious hole.
func transcriptsUnder(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // an uninitialized project directory is not an error
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// slugify reproduces Claude Code's project-directory naming: every
// non-alphanumeric character becomes a dash.
func slugify(path string) string {
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
