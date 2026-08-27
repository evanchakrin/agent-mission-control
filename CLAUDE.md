# Agent Mission Control — working notes for Claude

## Project invariants (do not break these)

- **Zero dependencies.** Node builtins only (`http`, `fs`, `path`, `os`, `crypto`, `child_process`). No npm packages, ever.
- **Watch-only.** The dashboard never launches, steers, or kills an agent. This is a permanent design stance and the core of the product's safety story.
- **Relays are outbound-only.** The hub must never open a connection to a relay. Directive distribution happens through **git**, not a hub→relay push channel — see the rejected "relay mailbox" design.
- **Never write `settings.json` or hooks remotely, or in bulk.** Guidance files (`CLAUDE.md` / `AGENTS.md`) hold words an agent reads; settings files hold commands the machine runs. Standing orders distribute words only. Hook edits stay deliberate, local, and one at a time through the Brain tab.
- **Everything that mutates goes through `writeGate`** (loopback + same-origin + `X-MC-CSRF`) and calls `appendAudit`. Files are snapshotted before they are touched.
- **Shell safety:** `execFile` with an explicit argv array. Never `exec`, never `shell: true`, never a path interpolated into a command string.

## Voice

The owner is non-technical. Every user-facing string is plain language — say what happens, not what it's called. Understatement and honest disqualification beat marketing. If a number is an estimate, say so where it appears.

<!-- mission-control:directive:tiering -->
## 🛰 Standing order: tier your models — set them explicitly, never inherit

_Planted by Agent Mission Control. Retire it from the dashboard rather than hand-editing this block._

**Set `model` explicitly on EVERY agent call. Never let a subagent inherit the orchestrator's model.** Omitting `model:` is silent, and it is where cost actually leaks — not in how many agents you spawn.

Measured across five weeks of this fleet's own transcripts (1,418 files, counted exactly — not sampled): **about 90% of agent launches name no model at all**, so they inherit the orchestrator's. One verified 18-agent workflow ran every single agent on the premium tier. The cheap tiers are effectively unused — Sonnet is ~1% of spend and Haiku essentially none.

Tier by the work being done, not by who spawned it:

- **Orchestrator, final synthesis, adversarial verdict, security-sensitive implementation** → premium (Opus 5). Flagship (Fable 5) only for the hardest long-horizon planning.
- **Review, research, drafting, analysis workers** → `claude-sonnet-5`.
- **Pure fetch / grep / read / summarize helpers** → `claude-haiku-4-5`.

Delegation only saves money when the workers run a **cheaper tier than the orchestrator**. Fanning out to same-tier subagents saves nothing — it just spends faster.

When a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**, not the ones intended. Configured and actual are different things; only the second one is billed. Check before claiming a tiering strategy worked.
<!-- /mission-control:directive:tiering -->
