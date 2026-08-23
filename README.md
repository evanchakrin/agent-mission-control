# Agent Mission Control

A live dashboard that visualizes Claude Code agent sessions — the orchestrator, its subagents, and workflow agent fleets — straight from the JSONL transcripts Claude Code writes to disk. Zero dependencies; one Node server, one page.

![status](https://img.shields.io/badge/deps-none-5eead4) ![node](https://img.shields.io/badge/node-%E2%89%A518-818cf8)

## Quick start

```bash
npx github:evanchakrin/agent-mission-control
# → http://localhost:4173
```

Or clone and run directly — requires Node 18+, zero dependencies:

```bash
node server.js [--port 4173] [--dir /path/to/.claude/projects]
```

## What it shows

- **Fleet view** (home) — every session on the machine as a card: agent count, events, tool calls, duration, tokens in/out, estimated cost in dollars, and error count. Click through to any session.
- **Board view** — a card per agent (orchestrator + every subagent) with live status (working / done / idle), current task, event count, output tokens, duration, estimated cost, error badges, tool-usage chips, and a 24-bucket activity sparkline. Edges from the orchestrator glow and run a pulse dot while an agent is active. Above 10 subagents it switches to a compact scrollable fleet grid, clustered by workflow run.
- **Timeline view** — Gantt-style swimlanes, one per agent, with **true duration bars** (each tool call paired with its result timestamp), color-coded by kind (errors in red), a time axis, and a scrub cursor. Click any block to seek; hover for the call's duration.
- **Replay export** — one click downloads a self-contained HTML file with the session data baked in: full board, timeline, playback, and inspector, no server needed. Send it to anyone.
- **Event feed** — live-scrolling feed of every message, tool call, result, and spawn, with free-text filtering and per-kind toggles. Click an event for a full-text inspector drawer.
- **Playback** — replay any session like a video at 1×/4×/16×, or hit LIVE to tail it in real time.
- **Stat bar** — session-wide agents, events, tool calls, duration, tokens in/out, estimated cost, and error count.

Cost estimates use published Claude API rates per model (cache reads billed at 0.1× input) and are approximate — cache writes and some surcharges aren't in the transcripts.

## How it works

Claude Code writes transcripts under `~/.claude/projects/<project-slug>/`:

| Path | Contents |
| --- | --- |
| `<sessionId>.jsonl` | the main (orchestrator) transcript |
| `<sessionId>/subagents/agent-*.jsonl` | one transcript per background subagent |
| `<sessionId>/subagents/workflows/<wf_runId>/agent-*.jsonl` | workflow-spawned agents |
| `<sessionId>/workflows/wf_*.json` | workflow metadata (names come from here) |

The server tails all of these (700 ms polling, whole-parse cached by a size signature over every file) and streams normalized events to the browser over Server-Sent Events. Subagents are matched to the `Agent`/`Task` calls that spawned them by the agent id embedded in the tool result, with timestamp-order pairing as a fallback; legacy inline `isSidechain` transcripts are also supported.

## Command center views

The overview has four lenses, all color-coded by agent type (Claude coral, Codex blue, OTLP violet):

- **Fleet** — session cards with a search box and filters (agent type, machine).
- **Table** — every session as a sortable row (click any column header); same filters.
- **Galaxy** — a force-directed constellation: each machine is a sun, each session a star orbiting it, sized by cost and glowing when recently active. Drag stars, scroll to zoom, click to open.
- **Machines** — one card per machine with its IP addresses, live/idle status, session and agent counts, cost, and agent-type breakdown.

## Beyond Claude Code: OpenTelemetry ingestion

The server is also an OTLP/HTTP trace receiver. Any OpenTelemetry-instrumented agent framework (CrewAI, LangGraph, AutoGen, OpenLIT, custom SDK code) can stream spans to it, and the session appears live in the Fleet view alongside your Claude Code sessions — same board, timeline, playback, and cost analytics.

Point your instrumentation at the dashboard with JSON protocol:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4173
export OTEL_EXPORTER_OTLP_PROTOCOL=http/json
```

Spans are mapped via the `gen_ai.*` semantic conventions: `service.name` names the session, `gen_ai.agent.name` groups spans into agent cards, `gen_ai.request.model` + `gen_ai.usage.*` drive token and cost accounting, `gen_ai.tool.name` becomes tool-call events with true durations, and span error status shows as errors. Only OTLP-JSON is accepted (no protobuf — the server has zero dependencies).

Try it with the bundled simulator while the dashboard is running:

```bash
node demo/simulate-crew.js
```

A three-agent "content-crew" (Researcher → Writer → Reviewer) streams in live over ~15 seconds.

## Codex sessions

Codex CLI / Desktop rollouts under `~/.codex/sessions` are discovered automatically and appear in the Fleet labeled `Codex` — including their named subagent teams (`sub_agent_activity` lanes, with each subagent's own rollout thread merged in). Tool calls are paired by `call_id` for durations, and `token_count` events drive token accounting. Very large rollouts (hundreds of MB) are tail-read with a 24MB cap and events capped at 15k, keeping the recent history. Override the location with `--codex-dir` or `CODEX_DIR`. Relay mode forwards Codex sessions to the hub too.

## Multi-machine: relay mode

Run one dashboard (the hub). Every other machine relays its sessions to it:

```bash
# hub (your main PC) — start with a shared secret
node server.js --token <secret>

# every other machine — no UI, just forwards its local sessions to the hub
npx github:evanchakrin/agent-mission-control --relay http://<hub-ip>:4173 --token <secret> --name office-pc
```

Relayed sessions appear in the hub's Fleet labeled `⇄ <machine>`. The token guards both `/v1/relay` and `/v1/traces`; open TCP port 4173 on the hub's firewall for remote machines.

## Install as a background service (Windows)

```bash
npx github:evanchakrin/agent-mission-control --install [--token <secret>] [--relay <hub> ...]
```

Copies itself to LocalAppData, starts hidden now and at every login, and (hub mode) drops an "Agent Mission Control" shortcut on the Desktop. No admin needed. Remove with `--uninstall`.

## Notes

- Each machine visualizes its own `~/.claude/projects` — run the server wherever the sessions run. Remote Control sessions work (they execute locally); cloud sessions don't (their transcripts never touch your disk).
- Transcripts contain your full conversations with Claude. The server binds locally and adds no auth — don't expose the port beyond machines you trust.
