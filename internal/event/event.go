// Package event defines the structured events that participants write to a
// shared session log.
//
// The type set is intentionally small. Every type earns its place by changing
// how the assembler treats it: priority, decay rate, or whether it occupies a
// reserved slot in the projection.
package event

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Type string

const (
	// Finding is an observed fact. Subject carries where it was observed.
	Finding Type = "finding"
	// Decision is a choice plus its rationale. Rejected alternatives belong here too.
	Decision Type = "decision"
	// Question is an unresolved gap. It stays open until an Answer refs it.
	Question Type = "question"
	// Answer closes a Question. Refs must contain the question's event ref.
	Answer Type = "answer"
	// Task is a declaration of work to be done.
	Task Type = "task"
	// Artifact points at something produced: a path, a PR, a URL.
	Artifact Type = "artifact"
	// Status is "what I am doing right now". Volatile: decays out of the
	// projection within the hour.
	Status Type = "status"
	// Note is everything else.
	Note Type = "note"
)

var allTypes = []Type{Finding, Decision, Question, Answer, Task, Artifact, Status, Note}

// AllTypes returns the valid event types, in descending assembler priority.
func AllTypes() []Type { return append([]Type(nil), allTypes...) }

// Priority is the assembler's base weight per type. A decision outranks a
// finding because it is not re-derivable: the reasoning that produced it is
// gone once the session that made it ends.
var priority = map[Type]float64{
	Decision: 10,
	Question: 9,
	Finding:  7,
	Answer:   6,
	Artifact: 5,
	Task:     4,
	Status:   3,
	Note:     2,
}

// halfLife controls how fast an unread event loses value. Status is aggressive:
// a stale "what I'm doing now" is worse than nothing, because it describes a
// world that has already moved on.
var halfLife = map[Type]time.Duration{
	Status: 30 * time.Minute,
	Note:   12 * time.Hour,
}

const defaultHalfLife = 72 * time.Hour

func (t Type) Valid() bool {
	_, ok := priority[t]
	return ok
}

func (t Type) Priority() float64 { return priority[t] }

func (t Type) HalfLife() time.Duration {
	if h, ok := halfLife[t]; ok {
		return h
	}
	return defaultHalfLife
}

// Event is one entry in the session log. Events are immutable once written;
// corrections are made by posting a new event with Supersedes set.
type Event struct {
	Seq     int64     `json:"seq"`
	Session string    `json:"session"`
	TS      time.Time `json:"ts"`
	Actor   string    `json:"actor"`
	Type    Type      `json:"type"`
	Subject string    `json:"subject,omitempty"`
	Body    string    `json:"body"`
	// Refs are "kind:value" pointers, e.g. "event:142", "file:src/auth/jwt.go",
	// "url:https://...". Free-form by design; only "event:" is interpreted.
	Refs       []string `json:"refs,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Supersedes int64    `json:"supersedes,omitempty"`
	EstTokens  int      `json:"est_tokens"`
}

func (e *Event) Validate() error {
	if !e.Type.Valid() {
		return fmt.Errorf("unknown event type %q (want one of %v)", e.Type, allTypes)
	}
	if e.Body == "" {
		return fmt.Errorf("body must not be empty")
	}
	if e.Actor == "" {
		return fmt.Errorf("actor must not be empty")
	}
	if e.Type == Answer && len(e.EventRefs()) == 0 {
		return fmt.Errorf("an answer must ref the question it closes (refs: [\"event:123\"])")
	}
	return nil
}

// Normalize canonicalizes an event in place. It exists because the link between
// an answer and its question is a string comparison in SQL: "event: 001" and
// "event:1" must not be two different things, or an answer validates, posts,
// and closes nothing.
func (e *Event) Normalize() {
	e.Type = Type(strings.ToLower(strings.TrimSpace(string(e.Type))))
	e.Subject = strings.TrimSpace(e.Subject)
	e.Body = strings.TrimSpace(e.Body)
	for i, r := range e.Refs {
		kind, val, ok := strings.Cut(r, ":")
		if !ok {
			e.Refs[i] = strings.TrimSpace(r)
			continue
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		val = strings.TrimSpace(val)
		if kind == "event" {
			if n, err := strconv.ParseInt(strings.TrimPrefix(val, "#"), 10, 64); err == nil && n > 0 {
				e.Refs[i] = "event:" + strconv.FormatInt(n, 10)
				continue
			}
		}
		e.Refs[i] = kind + ":" + val
	}
}

// EventRefs returns the seqs this event points at via "event:N" refs.
// Call Normalize first; this deliberately accepts only the canonical form so it
// cannot disagree with the SQL that does the same match.
func (e *Event) EventRefs() []int64 {
	var out []int64
	for _, r := range e.Refs {
		val, ok := strings.CutPrefix(r, "event:")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// EstimateTokens approximates the context cost of a string without pulling in a
// tokenizer. ASCII runs at roughly 4 chars per token; CJK closer to 1.3. The
// assembler only needs the ratio between events to be sane, not the absolute
// number to be exact.
func EstimateTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return ascii/4 + (wide*3)/4 + 1
}

// Cost is the projected token cost of rendering this event, including its
// header line.
func (e *Event) Cost() int {
	if e.EstTokens > 0 {
		return e.EstTokens
	}
	return EstimateTokens(e.Body) + EstimateTokens(e.Subject) + 12
}
