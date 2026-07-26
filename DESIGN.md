# copresence — a shared session substrate for AI agents

English | [日本語](DESIGN.ja.md)

> Co-presence: being in the same place at the same time. Agents do not send each
> other instructions; they share a place. The naming history and how this
> differs from adjacent products are in §13.

## 1. What this solves

Today's multi-agent setups are built on **A handing a prompt to B**.

- B starts cold. A's context is summarized into an instruction — a lossy,
  one-way compression.
- Whatever B saw along the way is invisible to everyone until B returns.
- Who decided what, and on what evidence, is recorded nowhere.

copresence **replaces message passing with reading and writing a shared world
state**. The lineage is the blackboard architecture plus event sourcing, with a
context budget added on top.

**Instructions disappear.** A does not ask B for anything: A writes what it
learned, B reads what is new to it. Nobody takes orders from anybody.

## 2. Scope

### In scope for v0
- Context sharing between agents running concurrently on one machine
- Exposed as an MCP server, so any MCP-capable runtime can join
- An append-only log of structured events
- A budgeted context projection (`catchup`)

### Out of scope for v0
- Edit coordination, locking, worktree management — left to existing git practice
- Network sync, team sharing, authorization — v1 or later
- LLM summarization — the projection stays deterministic (§6)
- Auto-capture (recording every tool call via hooks) — a noise source; explicit
  posting is the model

## 3. Architecture

```
 Agent A (Claude Code)   Agent B (Cursor)   Agent C (Codex)
        │                      │                  │
   MCP server            MCP server         MCP server     ← one per process
        └──────────────────────┼──────────────────┘
                               │
                  .copresence/session.db (SQLite WAL)
```

**No daemon.** SQLite in WAL mode handles multi-process concurrency, so there is
no resident process — no lifecycle to manage, no orphans, no socket paths. A
pull-based `catchup` is enough for notification; at one machine's human-paced
concurrency, even polling is unnecessary.

`busy_timeout` is generous. Several processes write at once, and blocking
briefly beats surfacing `SQLITE_BUSY` to a model that has no idea what to do
with it.

State lives in `.copresence/` at the workspace root, containing a `.gitignore`
of `*` plus `!.gitignore` so the directory **ignores itself** — running `init`
never dirties the user's own `.gitignore`.

## 4. Data model

```sql
CREATE TABLE events (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,  -- total order, assigned by SQLite
  session    TEXT    NOT NULL,                   -- 'main' by default
  ts         TEXT    NOT NULL,
  actor      TEXT    NOT NULL,
  type       TEXT    NOT NULL,
  subject    TEXT    NOT NULL DEFAULT '',        -- e.g. 'src/auth/jwt.go:88'
  body       TEXT    NOT NULL,
  refs       TEXT    NOT NULL DEFAULT '[]',      -- JSON ["kind:value", ...]
  tags       TEXT    NOT NULL DEFAULT '[]',
  supersedes INTEGER REFERENCES events(seq),
  est_tokens INTEGER NOT NULL
);
-- plus participants, watermarks, events_fts (FTS5 external content)
```

`refs` holds **"kind:value" strings** such as `"event:142"`,
`"file:src/auth/jwt.go"`, or `"url:..."`. Strings rather than structs because an
agent has to write them as a tool argument. Only two kinds are interpreted:
`event:` (linking an answer to its question) and `file:` (the reverse lookup
behind `session_context`). Everything else passes through untouched.

### Event types (deliberately few)

| type | meaning | priority | half-life |
|---|---|---|---|
| `decision` | a choice and why, including what was rejected | 10 | 72h |
| `question` | an unresolved gap; stays open until answered | 9 | 72h |
| `finding` | an observed fact; `subject` carries the location | 7 | 72h |
| `answer` | refs a question and closes it | 6 | 72h |
| `artifact` | a pointer to something produced | 5 | 72h |
| `task` | a declaration of work to be done | 4 | 72h |
| `status` | what you are doing right now | 3 | **30m** |
| `note` | everything else | 2 | 12h |

**Priority tracks how un-rederivable something is.** A finding can be
rediscovered by reading the code again; the reason a path was rejected is gone
the moment the session that decided it ends. Hence decision at the top.

**Only status decays on a radically shorter half-life.** A three-hour-old "what
I'm doing now" describes a world that has already moved on — worse than nothing.

**`supersedes`** is how corrections work. A wrong finding would otherwise haunt
the session forever; a superseding event retires it across every read path at
once (unread / search / context / digest / export).

## 5. MCP surface

```
session_post(type, body, subject?, refs?, tags?, supersedes?)
session_catchup(budget_tokens?, focus?)     ★ the core
session_search(query?, type?, subject?, limit?)
session_context(subject, limit?)

resource: session://digest
prompt:   join
```

Four tools, and no more. More tools compete for the model's attention, and
**a coordination tool that never gets called is worse than none**.

`actor` is held by the **MCP server process**, not taken from a tool argument,
so a participant cannot post as someone else.

## 6. The assembler — the actual product

`catchup(budget_tokens, focus?)`:

0. **Read the head (`MaxSeq`) first**, and bound every subsequent read by
   `seq <= head`. Skip this and an event written by another process mid-assembly
   becomes "marked read but never delivered" (§11).
1. Fetch unread above the watermark and at or below the head (**excluding your
   own events** — you already know them).
2. Drop superseded events.
3. Reserve a **fixed frame** first (capped at 30% of budget):
   - **every open question**, oldest first — drop these and each participant
     independently rediscovers the same hole
   - **each other participant's latest status** (within 2h) — the collision detector
4. Score the rest by `type priority × time decay × focus boost` and fill greedily.
   - **Skip rather than stop when something does not fit.** A small finding
     behind an enormous decision still gets in, which amounts to density-based
     packing.
5. Fold the overflow into per-type counts **and seqs** → `note×28 (#5-#32)`.
   Without the seqs, "recoverable via search" is a claim with no key to search on.
6. Advance the watermark — **only after the projection succeeded**. On a backlog
   larger than one page (500 events), advance **only to the last delivered seq**;
   advancing to the head would mark the remainder read unseen.

**No LLM summarization.** Deterministic, fast, dependency-free — and an
overflowing session degrades in a documented order rather than having a
summarizer silently drop the one line that mattered. Anything folded stays
reachable through search and context.

The fixed frame carries a symmetric risk: fifty open questions would push out
all new material. Hence the 30% cap, with the excess reported as a count. Both
directions are pinned by tests.

## 7. Trust boundary

A shared log is plumbing for prompt injection. If A is compromised, B and C are
compromised in turn by reading its findings.

- Output from catchup / search / context is always wrapped in `<session-events>`
  and states that this is data written by other agents, not instructions
- Every line carries provenance (`#seq`, `type`, `actor`)
- The `join` prompt spells out the rules
- If v1 adds network sync, this becomes the largest design debt

## 8. Implementation

Go: a single binary, `modernc.org/sqlite` so no cgo, and the official
`modelcontextprotocol/go-sdk`.

```
cmd/copresence/      CLI (init, mcp, catchup, post, log, search, context,
                          digest, export, usage, doctor)
internal/event/      types, priority/half-life, validation, token estimation
internal/store/      SQLite, migrations, every query, the usage table
internal/assemble/   ★ projection + digest
internal/mcpserver/  MCP server
internal/usage/      rate table, cost computation, transcript import
```

Table-driven tests are stacked heavily on `internal/assemble` and
`internal/store`. A regression there is a regression in the product itself.

## 9. Usage and cost

Records **what each agent spent**, designed for a dashboard to read.

**It does not go in the event log.** This is telemetry, not shared knowledge. No
agent needs to read another's bill, and putting it in the log would spend
catchup budget on rows nobody reads. It gets its own `usage` table.

```sql
CREATE TABLE usage (
  session, actor, ts, source, external_id,   -- (source, external_id) is UNIQUE
  model, speed, subagent,
  cwd, run_id,                               -- what the spend is attributable to
  input_tokens, output_tokens, cache_read_tokens,
  cache_write_5m, cache_write_1h,
  cost_usd, priced
);
```

### Attributing spend to work

"What did this piece of work cost" needs a unit of work. The obvious way to get
one is to have agents declare when they start and finish — which is exactly the
write-forgetting risk of §12, the biggest one this project has. Measured on this
project's own log: **two `status` events out of 27**, despite `status` being
documented and paying the agent that posts it back immediately in collision
avoidance. A declared marker that is kept half the time is worse than none,
because the resulting numbers look precise.

So attribution uses only **byproduct signals** — things recorded because the
work happened, not because anyone remembered to say so. They are complete or
absent, never partial:

| column | unit | how it is obtained |
|---|---|---|
| `cwd` | the directory worked in | present on every transcript line |
| `run_id` | one invocation of a runtime, first prompt to last | the runtime's own session id |
| `subagent` | main loop vs delegated | the transcript's path |

`run_id` is deliberately not called `session_id`: `session` already means the
copresence session, and one of those outlives many runs. It is qualified by
`source` when grouped, so a second runtime's ids cannot merge into a first's.

**Delegated agents inherit their caller's run**, because Claude Code writes the
parent's `sessionId` into the subagent transcript. A run's cost therefore
includes the work it handed off, while `subagent` still separates the two within
it.

The dimensions cut across each other, which is why more than one is kept: a
single run of this project's own sessions spanned three directories, and 13% of
what first looked like this repository's spend belonged to sibling projects.

**Cache pricing has three tiers**, all relative to the input rate: read = 0.1×,
5-minute cache write = 1.25×, 1-hour cache write = **2×**. Collapsing 5m and 1h
understates the write line of a cache-heavy workload by 60%. A Claude Code
transcript already splits them as
`cache_creation.ephemeral_{5m,1h}_input_tokens`, so use that.

**Cost is computed at import and stored.** A later price change does not move
what history says.

**Rates are resolved against the event's own timestamp.** Introductory pricing
exists (Sonnet 5 at $2/$10 through 2026-08-31), so re-importing an old
transcript reproduces what it actually cost.

**Fast mode is not inferable from the model id.** It is detected via
`usage.speed == "fast"`, where Opus 5 / 4.8 bill at $10/$50.

**An unknown model is flagged `priced=false`, not silently $0.** Better for a
dashboard to show "unpriced" than for a total to quietly omit it.

### Importing Claude Code transcripts

Reads `~/.claude/projects/<slug>/*.jsonl`. Two traps found by measurement:

1. **Deduplicating by `uuid` inflates the numbers 2.2×.** The same assistant
   message is rewritten across several lines, each carrying the identical final
   usage. **Deduplicate by `message.id`.**
2. **Transcripts are slugified by the session's cwd, not the repo root.** A
   session opened in a parent directory files under the parent's slug. The
   importer walks up ancestors and reports which directory it used, since an
   ancestor's transcripts can include sibling projects.

`UNIQUE(source, external_id)` makes import idempotent, which matters because
transcripts grow and the same file is read repeatedly.

### Commands

```
copresence usage import [FILE...]   # import (auto-discovers by default)
copresence usage [--by actor|model|day|scope|project|run|source]
                 [--since 7d] [--run ID] [--all-projects] [--json]
copresence usage records --limit N  # raw JSON — the dashboard feed
```

Reports scope to the workspace by default. `--run` matches on a prefix, because
run ids are UUIDs and nobody types one.

## 9b. Dashboard

`copresence dashboard` serves the session read-only on loopback: participants
and how far each has read, open questions, decisions, the timeline, and spend
across every dimension above.

**It is not a cost dashboard.** [ccusage](https://github.com/ryoppippi/ccusage)
already parses transcripts and prices them across many more runtimes. The thing
only copresence can put on one page is the spend next to the session it bought.

Constraints that fall out of what the database contains — the workspace path and
everything the agents said about the code:

- **One `GET /api/state`.** Four panes polled separately would disagree about
  the head; one snapshot cannot.
- **No write routes exist.** Not "protected" — absent.
- **Loopback only, not configurable.** Requests whose `Host` is not loopback are
  refused, which is what stops DNS rebinding: the attacker's page reaches the
  port but still sends their hostname. `Sec-Fetch-Site` rejects cross-site reads.
  `http.CrossOriginProtection` does not help here — it exempts safe methods by
  design, and every route is a GET whose *response* is the asset.
- **Assets are embedded and self-contained.** No CDN, so it works offline.
- **The page builds DOM with `textContent`.** Event bodies are agent-written
  text; the way to be sure none of it parses as markup is to never hand it to a
  parser.
- **A run filter narrows spend only**, and says so on screen: events carry no
  run id, so silently emptying the timeline would read as "this run did
  nothing".

## 10. Current state (v0)

M0–M2 complete, review findings fixed. Working:

- SQLite schema + FTS5 (trigram), supersedes excluded on every read path,
  monotonic watermarks
- Assembler complete: fixed frame, scoring, skip-fill, folding, determinism, and
  zero loss under concurrent writes
- MCP server: 4 tools + digest resource + join prompt, verified over stdio
- usage: import, aggregation, JSON feed
- CLI: 11 commands
- Tests: assembler 10, store 14, concurrency 3, usage 9

## 11. What implementation taught us

**Actor id collisions are fatal.** Two processes started with the same `--as`
filter each other's events out as their own, and sharing breaks silently. Ids
are now auto-generated per process when unspecified; a stable id is an opt-in
for preserving your read position across restarts.

**`answer` now requires a ref.** An answer with no ref looks resolved to a human
while the question stays open forever. Validation rejects it.

**Go's `flag` package silently swallows flags that follow the body.** The CLI's
`post --as me "body" --tag x` broke, so it is now an explicit error.

**A review by a separate session found that the daemonless bet was losing.**
`Unread` and `MaxSeq` were separate queries, so events written by another
process in between were "marked read but never delivered" — measured at 27 of
300 lost (9%), silently. Fixed by reading the head first and bounding on
`seq <= head`. **That one finding justified the concurrency tests on its own**:
the existing suite could not detect it, because the fake's `MaxSeq` behaved
differently from the real store.

**The supersedes guard documented as "applies to every read path" missed one.**
The answer side had no guard, so a retracted answer kept its question closed.
The cause was `OpenQuestions` being absent from the test's check map.

**FTS5's `unicode61` does not segment Japanese.** This project's own log is in
Japanese, so "folded content is recoverable through search" was not actually
true. Switched to `trigram`, with a `LIKE` fallback for queries under three
characters.

**The first thing a model searches for is a file path** — and a path is a string
full of FTS5 operators, so a raw query yields `syntax error near "/"`. **A search
should return zero rows, never an error.**

## 12. Open problems

- **Write-forgetting (the biggest risk).** Structured events depend on explicit
  posting, so the log is empty if agents do not write. Tool descriptions and the
  skill nudge toward it, but this needs measurement. If it fails, the
  "no auto-capture" stance in §2 is what gets revisited.
- **Session granularity.** One `main` per repo today (`--session` splits it).
  Whether feature branches or investigations want their own is unproven.
- **Compaction.** What happens at thousands of events. The sketch is a synthetic
  snapshot event condensing prior decisions, which new participants read first.
- **Participant sprawl.** Auto-generated ids add a row per restart. Needs cleanup.

## 13. Naming and adjacent products

This was designed under the name `sessionbus` until `Jacobious52/sessionbus`
(Rust, ★0) turned out to be **the same name and a near-identical concept**:
local-first, SQLite, MCP server, deterministic context packer.

The collaboration topology differs, though:

| | Jacobious52/sessionbus | copresence |
|---|---|---|
| Premise | one human moving between Codex → Cursor → Claude, avoiding re-explanation | several agents running **concurrently**, reading each other's findings |
| Unit | engineering task / intent | finding / decision / open question |
| Implementation | daemon + dashboard | daemonless |

Sequential handoff versus concurrent co-presence: adjacent, not identical. Even
so, sharing a name in the same genre is worth avoiding, hence the rename.

`copresence` was not chosen only for availability. `bus` connotes message
passing, pointing the opposite way from §1's central claim that the passing of
instructions is what we are removing. Co-presence names the topology itself. The
MCP tool names stay `session_*` — what a participant addresses is still a
session, and there is no reason to bend that to the product name.

Other prior art: MCP is vertical (agent↔tool); A2A is horizontal but still
message passing; AutoGen and CrewAI are orchestrator-centric; Letta and mem0 are
single-agent memory. **Runtime-agnostic × local-first × concurrent agents** looks
like open space.
