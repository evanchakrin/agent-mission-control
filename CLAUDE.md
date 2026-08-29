# Agent Mission Control — working notes for Claude

## Project invariants (do not break these)

- **Prefer zero dependencies.** Node builtins only (`http`, `fs`, `path`, `os`, `crypto`, `child_process`) is the default and has held so far. It is a strong preference, not a commandment: the project exists to serve its owner, and if a dependency clearly serves him better, take it and say why. Tools already on the machine (the C# compiler that ships with Windows, for example) are not dependencies.
- **Watch-only.** The dashboard never launches, steers, or kills an agent. This is a permanent design stance and the core of the product's safety story.
- **Relays are outbound-only.** The hub must never open a connection to a relay. Directive distribution happens through **git**, not a hub→relay push channel — see the rejected "relay mailbox" design.
- **Never write `settings.json` or hooks remotely, or in bulk.** Guidance files (`CLAUDE.md` / `AGENTS.md`) hold words an agent reads; settings files hold commands the machine runs. Standing orders distribute words only. Hook edits stay deliberate, local, and one at a time through the Brain tab.
- **Everything that mutates goes through `writeGate`** (loopback + same-origin + `X-MC-CSRF`) and calls `appendAudit`. Files are snapshotted before they are touched.
- **Shell safety:** `execFile` with an explicit argv array. Never `exec`, never `shell: true`, never a path interpolated into a command string.

## Voice

The owner is non-technical. Every user-facing string is plain language — say what happens, not what it's called. Understatement and honest disqualification beat marketing. If a number is an estimate, say so where it appears.

<!-- mission-control:directive:tiering -->
## 🛰 Standing order: tier your models, and watch how long agents live

_Planted by Agent Mission Control. Retire it from the dashboard rather than hand-editing this block._

**Set `model` explicitly on EVERY agent call. Never let a subagent inherit the orchestrator's model.** Omitting `model:` is silent, and inheritance is how a cheap job ends up billed at the orchestrator's rate.

Everything below is measured from this fleet's own transcripts — **223 sessions, 1,397 subagents, two machines** — recounted after three cost-accounting bugs were fixed (duplicate replies, unbilled cache writes, and agents priced at whichever model spoke first). Earlier versions of this note quoted figures produced by that broken accounting; these replace them.

**Tier, by the work being done rather than by who spawned it:**

- **Orchestrator, final synthesis, adversarial verdict, security-sensitive implementation** → premium (Opus 5). Flagship (Fable 5) only for the hardest long-horizon planning.
- **Review, research, drafting, analysis workers** → `claude-sonnet-5`.
- **Pure fetch / grep / read / summarize helpers** → `claude-haiku-4-5`.

Where the money actually sits: **96% of spend is on the top two tiers** (premium 66%, flagship 30%). Sonnet is 3.9%. Haiku is **$0.11 across the entire fleet** — the cheap tier is not underused, it is unused. At matched workload Sonnet costs **35–40% less than Opus 5** ($0.063 vs $0.103 per turn).

**Agent lifetime is the larger lever, and almost nobody sets it.** Cost per turn is U-shaped:

| turns an agent lives | cost/turn | cache written/turn | cache read/turn |
|---|---|---|---|
| 1–2 | **$0.434** | 63,692 | 19,485 |
| 6–10 | **$0.097** | 8,896 | 65,124 |
| 21–40 | $0.106 | 5,560 | 129,637 |
| 41+ | **$0.137** | 4,661 | 196,925 |

Two forces cross. Setup **amortizes** (64k → 4.7k tokens written per turn) and context re-reading **compounds** (19k → 197k read per turn).

- **Do not spawn one-shot subagents.** A 1–2 turn agent pays a full briefing and dies before earning it back — 4× the per-turn cost of a 10-turn one. If the job is a single grep or read, do it inline.
- **Cap long-running agents near 30 turns.** 71% of all subagent spend is in agents living 41+ turns, each re-reading ~197k tokens every turn. Splitting one costs two extra briefings and saves roughly three times that in re-reads.

Delegation saves money only when workers run a **cheaper tier** than the orchestrator **and live long enough to amortize their briefing**. Fanning out to same-tier one-shot agents is the worst case on both axes at once.

When a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**, not the ones intended. Configured and actual are different things; only the second one is billed. Check before claiming a tiering strategy worked.
<!-- /mission-control:directive:tiering -->
