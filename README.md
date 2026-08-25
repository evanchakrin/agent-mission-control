# Agent Mission Control

**The operator's cockpit for the age of many agents.** One Node server, one page, zero dependencies — a live dashboard that watches every Claude Code, Codex, and OpenTelemetry agent session across every machine you own, tells you what your fleet is actually doing, and turns what it learns into rules your future sessions inherit.

![status](https://img.shields.io/badge/deps-none-5eead4) ![node](https://img.shields.io/badge/node-%E2%89%A518-818cf8) ![license](https://img.shields.io/badge/license-MIT-6ee7b7)

```bash
npx github:evanchakrin/agent-mission-control
# → http://localhost:4173
```

---

## What we're really building

Coding agents went from "one chat window" to **fleets** — orchestrators spawning subagents, workflows fanning out dozens at a time, the same person running work on three machines at once. The tooling didn't keep up. You can watch *one* session in your terminal; you can't see the fleet, you can't compare last week to this week, and when a run burns \$70 you find out on the invoice.

Mission Control is the missing layer: **an analytics and intelligence surface that sits above whatever runs your agents.** It doesn't replace Claude Code or Codex or your orchestrator — it reads the transcripts they already write to disk and gives you the cockpit. It never launches or kills an agent, so it's safe to run against production work. It's self-hosted and dependency-free, so your transcripts never leave your machines.

### The five things it gives you

1. **See the whole fleet, live.** Every session on every machine on one page — orchestrator, subagents, workflow swarms — with real-time status, a topology board, and Gantt timelines built from true tool-call durations. Not a log tail; a control room.
2. **Know what everything costs — per model, per role, per machine.** Dollar estimates on every session and every agent, so the \$70 sweep is a number you see *while it happens*, not a line on next month's bill.
3. **Catch failures as failures.** Retrying, stalled, and failed agents are distinguished from healthy-but-slow ones — derived from the transcripts, no instrumentation required. You see *what work* was flowing on each edge, not just that an edge exists.
4. **Learn from your own history.** Playbook Studio mines your fleet for patterns — which roles keep failing, which burn premium tokens for no reason, which are rock-solid — and hands you fixes you can apply. (How it does this, and why it's honest, is [below](#playbook-studio--the-feedback-loop).)
5. **Own your data.** Zero dependencies, self-hosted, loopback-gated. Multi-machine relays are outbound-only — a remote machine can send its sessions to your hub but can never reach back in. Your conversations stay yours.

Everything below is how each of those works.

---

## Who it's for

Most dashboard READMEs try to convince everyone. This one tries to disqualify you quickly, because the fit is narrower than the feature list suggests.

**The pain starts the moment agent work leaves the visible terminal.** If you run one agent at a time in one window, you can already see it — you don't need this. Everything below assumes you can't.

### 1. Solo operators who delegate — the core audience

You run Claude Code with subagents, parallel sessions in different terminals, or long unattended runs. A subagent shows you a spinner and a token count; it can't tell you what it was asked to do or why it came back with garbage. What you'd actually use it for:

- *"That subagent returned nonsense — show me the exact task text the orchestrator handed it."*
- *"This run took 40 minutes. Find the one tool call that ate 11 of them."*
- *"Is that background session still moving, or did it stall 20 minutes ago — and is that a failure or a retry?"*
- *"Rank this week's sessions by estimated cost and tell me which project each belongs to."*
- *"My orchestrator delegated to subagents on the same premium tier — did that delegation actually buy me anything?"*
- *"Take that finding and write it into this repo's `CLAUDE.md` so the next session inherits it."*
- *"Scroll back to where it went off the rails three hours ago, without page-up 200 times."*

### 2. Solo operators running agents across several machines

A desktop, a laptop, a VPS. `ssh` + `tmux` shows you one live terminal and nothing else: no history, no cross-machine total, and nothing survives the box being reimaged. Smallest audience here, deepest pain, and the reason this project exists.

- *"One list of everything running right now, on every machine."*
- *"Did the overnight job on the desktop finish, fail, or wedge — without SSHing in to look."*
- *"Keep the VPS's raw transcripts on the hub so I still have them after I destroy the box."*
- *"Which machine keeps producing failed sessions — is this an environment problem, not a prompt problem?"*
- *"Put my Codex runs and my Claude Code runs in the same list."*

Honest friction: this one is **not** npx-and-done. It's a relay per machine, usually over Tailscale.

### 3. Framework builders already emitting OpenTelemetry

Point an existing exporter at `localhost` and get a topology and a waterfall in about a minute — no docker-compose, no collector config, no account, no signup. Good for the evening you're debugging a CrewAI or LangGraph run. It is **not** a Phoenix or Langfuse replacement: they own evals, datasets, and run-to-run comparison, and none of that exists here. The one thing nobody else gives you is your custom agents and the Claude Code session you wrote them in, on the same screen.

### It's not for you if…

- **You need team access.** Single-user by design — no auth, no RBAC, no multi-tenant, no hosted option. Two people cannot share a board.
- **You need accurate spend.** Every dollar figure is *estimated* from transcripts at published API rates. On a Pro or Max seat you aren't billed per token, so these describe API-equivalent cost, not your bill. The Anthropic Console is ground truth.
- **You need to enforce a budget.** This reports; it cannot cap, throttle, or block a run. A gateway like LiteLLM does that, with metered numbers.
- **You need quota forecasting.** It shows what a session cost afterward, not how long until your window resets. `ccusage` and Claude Code Usage Monitor are better at that.
- **You need evals, datasets, scoring, or prompt experiments.** None of that is here. Braintrust, LangSmith, Phoenix, and Langfuse are built for it.
- **Your agents run in the cloud.** Cloud-executed sessions never write transcripts to your disk, so there's nothing to read.
- **You want to launch, steer, or kill agents from the dashboard.** Watch-only, permanently, on purpose.
- **You need to redact before sharing.** Replay exports embed the raw session — code, file paths, and anything pasted into the terminal.
- **You already run Grafana/SigNoz/Datadog with Claude Code's native OTel export.** You have tokens, cost, and latency. What this adds is session structure, replay, and the write-back loop — not the metrics.

### What actually makes it stick

Charts are a first-week novelty; cost is a one-time shock. The thing here that nothing else does is the loop: **Playbook Studio finds a bad delegation pattern in your real history, and Standing Orders writes the fix into your `CLAUDE.md` so every future session inherits it.** That's the reason to keep it open — the rest is a competent version of things that exist elsewhere.

---

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

## Organizing sessions (projects, archive, pin, notes)

Every session card has a `⋯` menu: assign it to a **project**, **pin** it, **archive** it, or attach a **note**. The **Projects** view is a drag-and-drop board — drag sessions between colored project columns; create, rename, recolor, or delete projects. Fleet and Table gain an **Active / Archived / All** toggle and a **project** filter; pinned sessions sort first.

This metadata is **local and durable** — stored at `~/.claude/mission-control/state.json`, written atomically, and bound to a stable per-session key so it survives relay re-sends and hub restarts (it never rides on the volatile session file id). Organizing a session never triggers a reparse or touches the read-only transcript layer.

**Security:** all `/api/meta*` routes (reads and writes) are gated to loopback only, plus a same-origin check and a per-boot CSRF token on writes — a web page you visit cannot reach in and mutate your data, and a remote relay (even with the shared token) can POST session content but cannot touch your projects/archive. Metadata mutation is never exposed on the LAN.

## Understanding control flow

- **Waterfall** (in-session) — nested span tree: every tool call as a duration bar under its agent, with per-agent rollups (calls, tokens, cost, duration, errors), collapsible, click any span for the inspector.
- **Lifecycle states** — agents and Board edges distinguish **failed / retrying / stalled** from healthy-but-slow: retry counts are derived from errored-then-repeated tool calls, stalls from long-pending calls. Edge styles: green=returned, dashed=in-flight, red=failed, red-dashed=retrying, amber=stalled.
- **Typed, labeled edges** — each orchestrator→subagent edge carries the delegated task text, so the Board shows what work is flowing, not just topology.
- **⇶ Flows** (fleet-wide) — aggregates behavior across your whole fleet: most-delegated agent roles weighted by frequency (with error counts), and **trajectory clusters** grouping sessions that behave alike — outliers flagged. No single-run cloud tool can build this; it needs the cross-machine data only a local fleet hub has.

The OTLP receiver also understands OpenInference (Phoenix) and Traceloop/OpenLLMetry attribute conventions (`llm.model_name`, `openinference.span.kind`, `llm.token_count.*`, `input/output.value`) in addition to `gen_ai.*` — any app instrumented with those exporters works out of the box.

## Playbook Studio & the feedback loop

The Board and Timeline tell you what *one run* did. Playbook Studio answers the harder question: **across everything my fleet has ever done, what should I change?** — and then gives you concrete ways to make that change stick in future sessions.

### How the mining actually works (on-demand and local — not scripted, not periodic)

There is **no background job, no cron, and no LLM call.** When you open the **Playbooks** tab, the studio does this, in your browser:

1. Pulls the full parsed data for up to ~60 recent sessions from the hub (`/api/session`).
2. `mineFleet()` groups every subagent by **role** — the run number and hash suffixes stripped off the name, so `review #3` and `review #7` fold into one `review` role.
3. For each role it computes a success rate, total and per-run cost, the model tier it ran on, and a per-machine breakdown.
4. It fires a handful of **deterministic rules** and surfaces the ones that trip:
   - a role failing more than ~30% of the time → *investigate this role*,
   - a reliable role running only on premium models → *downgrade candidate*,
   - an orchestrator keeping the lion's share of the spend → *hoarding*,
   - a role that only works on one machine → *environment-dependent*.

That's it — same inputs, same findings, every time, computed locally and free. It recomputes when you open the tab or hit **re-study**; only your triage decisions (dismiss / act) persist. Nothing is sent anywhere. The determinism is deliberate: mining you can't see the logic of is mining you can't trust.

> The bundled starter playbooks were a separate one-time research pass, not the studio. The studio's own analysis is always the live, rule-based mine described above.

### Canonizing the loop: three feedback-forward pathways

A finding is worthless until a *future* session acts on it. Copy-pasting a prompt works, but it's the weakest link — you have to remember, every time. Mission Control canonizes three pathways instead, ordered by how durable they are:

| Pathway | How a learning lands | Best for | Effort per future session |
| --- | --- | --- | --- |
| **① Directive** | Written into the project's `CLAUDE.md` / `AGENTS.md` — a standing rule the agent reads every session | Stable truths (*"tier your models"*, *"never guess the schema"*) | Zero — it's automatic |
| **② Playbook** | Saved to your library, pasted when you start relevant work | Task-shaped patterns (*"adversarial review"*, *"migration sweep"*) | One paste, when relevant |
| **③ Hook** | A `SessionStart` hook that surfaces your top directive/insight to the agent | Enforcement you don't want to rely on memory for | Zero — it fires itself |

**Copy-paste is the universal bridge** — it works across machines, stays watch-only, and carries no execution risk — so it's always available as the fallback. But the durable path is **① directives**, and the automatic path is **③ hooks**.

Pathway ① is built in: every insight carries a **🛰 make it a rule** button that opens the composer, and planted rules live on the **Standing orders** board — each one drift-checkable (is it still in the file?), retirable in one click (the block is removed, never the file), snapshotted before every touch, and measured with an honest before/after top-tier-spend trend (small samples say so, instead of pretending). Local machine only, prose files only (`CLAUDE.md`/`AGENTS.md`, never settings or hooks), loopback + CSRF gated like everything else that writes.

The worked example is model tiering. Playbook Studio flagged that reliable review/research roles were inheriting the premium orchestrator model and burning tokens they didn't need. The fix became a **directive** dropped into four repos' `CLAUDE.md`:

```
Orchestrator / final synthesis / adversarial verdict → premium (Opus 5; Fable 5 only for the hardest planning).
Review / research / accuracy-check workers → claude-sonnet-5.
Pure fetch / grep / read / summarize helpers → claude-haiku-4-5.
Reserve premium tokens for planning and the final judgment gate. Report a per-tier cost breakdown.
```

Every future workflow in those repos now tiers its models without anyone remembering to ask. That's the loop closed: **observe → mine → canonize → the next session inherits it.**

## Command center views

The overview has four lenses, all color-coded by agent type (Claude coral, Codex blue, OTLP violet):

- **Fleet** — session cards with a search box and filters (agent type, machine).
- **Table** — every session as a sortable row (click any column header); same filters.
- **Galaxy** — a force-directed constellation: each machine is a sun, each session a star orbiting it, sized by cost and glowing when recently active. Drag stars, scroll to zoom, click to open.
- **Machines** — one card per machine with its IP addresses, live/idle status, session and agent counts, cost, and agent-type breakdown.

## Beyond Claude Code: any agent that speaks OpenTelemetry

**A waterfall in one minute, with no setup.** No Docker, no collector config, no account, no signup.

OpenTelemetry is the industry standard for software reporting what it just did — each step becomes a *span* (name, start, end, did it error), and a chain of spans is a *trace*. **OTLP** is the format those reports travel in. It's a standard plug, so anything already instrumented — CrewAI, LangGraph, AutoGen, OpenLIT, your own SDK code — can point its existing exporter here and show up live in the Fleet view with the same board, timeline, playback, and cost analytics as your Claude Code sessions. No integration written on either side.

> **Honest positioning:** this is not an [Arize Phoenix](https://github.com/Arize-ai/phoenix) or Langfuse replacement. They own evals, datasets, and run-to-run experiments, and none of that exists here. Use them for *"was this answer any good?"*. Use this when you want a topology and a waterfall right now without standing anything up — or when you want your custom agents and the Claude Code session you wrote them in on one screen, which is the part nobody else does.

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

Relayed sessions appear in the hub's Fleet labeled `⇄ <machine>`. The token guards both `/v1/relay` and `/v1/traces`; open TCP port 4173 on the hub's firewall for remote machines. **Relays are outbound-only by design** — the hub never opens a connection back to a relay, so a relayed machine exposes no inbound surface.

### Full-transcript archive (optional)

Add `--archive` to a relay and it also uploads the raw `.jsonl` transcripts themselves, not just live events — so the hub keeps a durable, browsable history of every session a machine has run, even after the machine is offline. Uploads are manifest-based (deduped by path + size), capped at 120 MB per file and 6 GB total, and land under `~/.claude/mission-control/archive/<machine>/`. Add `--archive-codex` to include Codex rollouts. Browse them from each machine's card in the **Machines** view.

## Install as a background service (Windows)

```bash
npx github:evanchakrin/agent-mission-control --install [--token <secret>] [--relay <hub> ...]
```

Copies itself to LocalAppData, starts hidden now and at every login, and (hub mode) drops an "Agent Mission Control" shortcut on the Desktop. No admin needed. Remove with `--uninstall`.

## Notes

- Each machine visualizes its own `~/.claude/projects` — run the server wherever the sessions run. Remote Control sessions work (they execute locally); cloud sessions don't (their transcripts never touch your disk).
- Transcripts contain your full conversations with Claude. The server binds locally and adds no auth — don't expose the port beyond machines you trust.
- **Watch-only by design.** Mission Control reads transcripts and never starts, stops, or steers an agent. It's an analytics layer, safe to point at production work.

## License

MIT © Evan Chakrin
