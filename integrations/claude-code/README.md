# Claude Code integration

## 1. Register the MCP server

```bash
cd your-repo
copresence init
claude mcp add copresence -- copresence mcp --as claude-1 --runtime claude-code --dir "$PWD"
```

Run a second Claude Code with `--as claude-2`. Two agents, one session.

Give every concurrent agent a **different** `--as`. Sharing one id breaks
sharing silently: each process filters the other's events out as its own.
Omitting `--as` generates a fresh id per process, which is safe but starts your
read position at zero each time.

## 2. Install the skill

```bash
cp -r integrations/claude-code/skills/session ~/.claude/skills/
```

This is the part that decides whether the whole thing works. Structured events
only exist if agents post them, and a model with four unfamiliar tools and no
guidance will use them inconsistently. The skill says when to post and — just as
importantly — when not to.

## 3. Optional: auto-catchup on session start

Add to `.claude/settings.json` so every new Claude Code session opens with the
shared state already in context, at zero cost to the user:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "copresence catchup --as claude-1 --budget 1200 --peek --dir \"$CLAUDE_PROJECT_DIR\" 2>/dev/null || true"
          }
        ]
      }
    ]
  }
}
```

`--peek` leaves the watermark alone, so the first real `session_catchup` inside
the session still shows this material rather than reporting nothing new. The
trailing `|| true` keeps a repo without `copresence init` from erroring on every
session start.

## What is deliberately not here

There is no `PostToolUse` hook that records every tool call. Auto-capture would
solve the write-forgetting problem by drowning it: the log would fill with
transcript noise and the assembler would spend its whole budget ranking it.
Deciding what is worth recording is the model's job.

If measurement shows agents simply do not post enough for the session to be
useful, that tradeoff is the first thing to revisit — see DESIGN.md §11.
