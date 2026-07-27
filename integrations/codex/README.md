# Codex integration

## 1. Register the MCP server

```bash
cd your-repo
copresence init
codex mcp add copresence -- copresence mcp --runtime codex --dir "$PWD"
```

This intentionally omits `--as`. Each MCP process gets a fresh participant id,
so two Codex instances can safely use the shared configuration at the same
time. The tradeoff is that each new process starts its read position at zero.

If a read position must survive restarts, add a stable `--as codex-1`. Give
every concurrent agent a **different** `--as`: sharing one id breaks sharing
silently because each process filters the other's events out as its own. A
fixed id in shared Codex configuration is therefore appropriate only when one
Codex instance uses that configuration at a time.

The CLI command writes to the user-level Codex configuration. Codex CLI, the
IDE extension, and the ChatGPT desktop app share that MCP configuration on the
same host.

For a repository-scoped setup instead, add the equivalent server to the
repository's trusted `.codex/config.toml`:

```toml
[mcp_servers.copresence]
command = "copresence"
args = ["mcp", "--runtime", "codex", "--dir", "/absolute/path/to/your-repo"]
```

Use an absolute repository path in project config. A relative `--dir` would be
resolved from the MCP process's working directory, which is not part of the
integration's contract.

## 2. Install the skill

Install it for your user:

```bash
mkdir -p "$HOME/.agents/skills"
cp -R integrations/codex/skills/session "$HOME/.agents/skills/"
```

Or check it into one repository:

```bash
mkdir -p .agents/skills
cp -R /path/to/copresence/integrations/codex/skills/session .agents/skills/
```

This is the part that decides whether the whole thing works. Structured events
only exist if agents post them, and a model with four unfamiliar tools and no
guidance will use them inconsistently. The skill says when to post and — just as
importantly — when not to.

Codex detects skill changes automatically. Restart Codex if the new skill does
not appear.

## 3. Optional: auto-catchup on session start

Add this to the target repository's `.codex/hooks.json` so new and resumed
Codex sessions open with the shared state already in context:

```json
{
  "description": "Load the copresence session when Codex starts.",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume",
        "hooks": [
          {
            "type": "command",
            "command": "copresence catchup --as codex-start-hook --budget 1200 --peek --dir \"$PWD\" 2>/dev/null || true",
            "timeout": 10,
            "statusMessage": "Loading shared session"
          }
        ]
      }
    ]
  }
}
```

Project hooks run only after the repository is trusted. Codex also requires
reviewing and trusting a new or changed command hook; use `/hooks` if Codex
reports that this hook is pending review.

`--peek` leaves the watermark alone, so the first real `session_catchup` inside
the session still shows this material rather than reporting nothing new. The
trailing `|| true` keeps a repo without `copresence init` from erroring on every
session start.

The hook uses its own observer id. Because it only peeks and never posts, it
does not collide with a Codex participant or alter anyone's read position.

## What is deliberately not here

There is no `PostToolUse` hook that records every tool call. Auto-capture would
solve the write-forgetting problem by drowning it: the log would fill with
transcript noise and the assembler would spend its whole budget ranking it.
Deciding what is worth recording is the model's job.

If measurement shows agents simply do not post enough for the session to be
useful, that tradeoff is the first thing to revisit — see
[DESIGN.md §11](../../DESIGN.md#11-what-implementation-taught-us).
