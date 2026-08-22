# Agent Mission Control

A live dashboard that visualizes Claude Code agent sessions — the orchestrator, its subagents, and workflow agent fleets — straight from the JSONL transcripts Claude Code writes to disk. Zero dependencies; one Node server, one page.

![status](https://img.shields.io/badge/deps-none-5eead4) ![node](https://img.shields.io/badge/node-%E2%89%A518-818cf8)

## Quick start

```bash
node server.js
# → http://localhost:4173
```

Requires Node 18+. Nothing to install — the server is plain Node core.

## What it shows

- **Board view** — a card per agent (orchestrator + every subagent) with live status (working / done / idle), current task, event count, output tokens, duration, error badges, tool-usage chips, and a 24-bucket activity sparkline. Edges from the orchestrator glow and run a pulse dot while an agent is active. Above 10 subagents it switches to a compact scrollable fleet grid, clustered by workflow run.
- **Timeline view** — Gantt-style swimlanes, one per agent, with color-coded event blocks (errors in red), a time axis, and a scrub cursor. Click any block to seek.
- **Event feed** — live-scrolling feed of every message, tool call, result, and spawn, with free-text filtering and per-kind toggles. Click an event for a full-text inspector drawer.
- **Playback** — replay any session like a video at 1×/4×/16×, or hit LIVE to tail it in real time.
- **Stat bar** — session-wide agents, events, tool calls, duration, tokens in/out, and error count.

## How it works

Claude Code writes transcripts under `~/.claude/projects/<project-slug>/`:

| Path | Contents |
| --- | --- |
| `<sessionId>.jsonl` | the main (orchestrator) transcript |
| `<sessionId>/subagents/agent-*.jsonl` | one transcript per background subagent |
| `<sessionId>/subagents/workflows/<wf_runId>/agent-*.jsonl` | workflow-spawned agents |
| `<sessionId>/workflows/wf_*.json` | workflow metadata (names come from here) |

The server tails all of these (700 ms polling, whole-parse cached by a size signature over every file) and streams normalized events to the browser over Server-Sent Events. Subagents are matched to the `Agent`/`Task` calls that spawned them by the agent id embedded in the tool result, with timestamp-order pairing as a fallback; legacy inline `isSidechain` transcripts are also supported.

## Notes

- Each machine visualizes its own `~/.claude/projects` — run the server wherever the sessions run. Remote Control sessions work (they execute locally); cloud sessions don't (their transcripts never touch your disk).
- Transcripts contain your full conversations with Claude. The server binds locally and adds no auth — don't expose the port beyond machines you trust.
