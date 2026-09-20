# Accounting evidence rules

The parser indexes bounded previews and full searchable text while every event
references its immutable raw record. Pricing is a separate versioned projection.

## Claude

Usage is keyed by source generation, agent and provider message ID. Later blocks
revise that observation; they are not added as new calls. Field presence is
merged transactionally by the store, including explicit zeroes. Missing model or
token buckets remain unknown until actually reported. No model is inherited from
another message. Claude documents that streamed message-delta usage is cumulative:
[Claude streaming documentation](https://platform.claude.com/docs/en/build-with-claude/streaming).

Text/tool duplicates with a native identity and identical contents are suppressed;
changed contents retain a new event and raw reference. Records without native IDs
are not deduplicated by text alone. Parent-message edges and explicit agent IDs are
retained. Legacy inline `isSidechain` records without agent IDs are explicitly
`sidechain:unresolved`, not silently labeled as the main agent. Resolving their
full historical ancestry requires a durable graph projection, not an unbounded
checkpoint map or a guessed nearest spawn.

## Codex

Independent cumulative scopes never use a cross-thread maximum. A model applies
only to its local thread, and a parent's model never labels a child counter.
The first observed total may include earlier history: a fitting reported last
call can be attributed to the current model, while the residual stays unpriced.
The same rule applies at model changes. There is no proportional allocation.

Counter decreases are ambiguous. The upstream implementation both accumulates
completed usage and can replace counters with context-window estimates. Therefore
decreases produce an explicit diagnostic and preserve the prior high-water
checkpoint, instead of inventing a new billable epoch. Subsequent records remain
raw evidence even when the parser cannot establish a complete billable delta.
See `TokenUsageInfo::append_last_usage` and `fill_to_context_window` in the
[Codex protocol source](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs).
Reasoning output is not added on top of output tokens. Quota-only `info:null`
events are valid and do not mean usage parsing failed.

Fork ancestry is retained separately from parent/child ancestry. Copied histories
without ownership markers cannot be proven to represent new billable calls;
cross-source inherited-history deduplication is not inferred from identical text.

Parser version 4 binds Codex ownership to the first nonempty `session_meta.id`,
not the filename hint or a later embedded parent header. Conflicting headers stay
indexed with a diagnostic but cannot overwrite owner identity, ancestry, project,
model, or switch the default cumulative-counter scope. Partial matching headers
do not erase established ancestry. Version-3 checkpoints require a staged rebuild;
continuing their already-corrupted scope state would not repair saved history.
This prevents within-source recounting caused by identity switches; it does not
resolve inherited usage across different fork sources. Such recorded totals are
not a verified unique billable fleet total.

Upstream's [thread persistence contract](https://github.com/openai/codex/blob/main/codex-rs/thread-store/src/types.rs)
explicitly uses the first session metadata record because later records can be
copied parent headers. It distinguishes the shared root `session_id` from the
owning thread `id`, and defines `history_base` and
`subagent_history_start_ordinal` for explicit inherited-history boundaries.
The inspected legacy-v2 child source has both boundary fields null. Therefore
that source cannot be split into inherited and new billable usage using those
fields; timestamps or a shared root session ID must not be substituted as proof.
This source-format check supports the ownership fix, not completed cross-source
accounting reconciliation or compatibility with every upstream history mode.

These rules deliberately distinguish recorded evidence from provider invoices.
Unknown models, missing buckets, ambiguous resets, malformed records and records
over the indexing memory budget must remain visible, never priced as exact.
