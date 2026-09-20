"""Create a bounded AMC statistics snapshot without copying chat content.

This is a migration preview, not a live hub database. It reads the source in a
single SQLite snapshot and never changes the source or its WAL.
"""

import argparse
import hashlib
import json
import sqlite3
from datetime import datetime, timezone
from pathlib import Path


SESSION_COLUMNS = (
    "id", "source_id", "generation", "projection_revision", "machine_id",
    "provider", "project", "native_id", "parent_native_id", "archived",
    "last_activity", "event_count", "tokens_in", "tokens_cache",
    "tokens_write", "tokens_out", "cost_estimate", "pinned", "pricing_known",
    "priced_tokens", "unattributed_tokens",
)
USAGE_COLUMNS = (
    "session_id", "source_id", "generation", "projection_revision",
    "observations", "tokens_in", "tokens_cache", "tokens_write",
    "tokens_out", "unknown_tokens",
)
STAT_TABLES = {
    "query_agent_usage": USAGE_COLUMNS[:4] + ("agent_id",) + USAGE_COLUMNS[4:],
    "query_model_usage": USAGE_COLUMNS[:4] + ("model",) + USAGE_COLUMNS[4:],
    "query_daily_usage": USAGE_COLUMNS[:4] + ("day",) + USAGE_COLUMNS[4:],
    "query_event_agents": (
        "session_id", "source_id", "generation", "projection_revision",
        "agent_id", "events", "tool_calls", "errors", "unknown_results",
        "indexing_errors", "first_at", "last_at",
    ),
    "query_flow_tools_v1": (
        "session_id", "source_id", "generation", "projection_revision",
        "tool", "calls",
    ),
    "current_comparisons": (
        "session_id", "snapshot_id", "generation", "projection_revision",
        "indexed_offset", "cost", "priced_tokens", "unattributed_tokens",
    ),
    "projects": ("id", "name", "color", "revision", "deleted", "updated_at"),
    "machine_labels": ("machine_id", "display_name", "revision", "updated_at"),
}
TOKEN_COLUMNS = ("tokens_in", "tokens_cache", "tokens_write", "tokens_out")


def columns(connection, table):
    return [(row[1], row[2] or "BLOB") for row in connection.execute(f"PRAGMA table_info({table})")]


def create_and_copy(source, target, source_table, target_table, selected=None):
    available = columns(source, source_table)
    fields = selected or tuple(name for name, _ in available)
    types = dict(available)
    if not fields or any(field not in types for field in fields):
        raise ValueError(f"missing columns in {source_table}")
    declaration = ",".join(f'"{field}" {types[field]}' for field in fields)
    target.execute(f'CREATE TABLE "{target_table}" ({declaration})')
    names = ",".join(f'"{field}"' for field in fields)
    reader = source.execute(f'SELECT {names} FROM "{source_table}"')
    statement = f'INSERT INTO "{target_table}" VALUES ({",".join("?" for _ in fields)})'
    count = 0
    while batch := reader.fetchmany(1000):
        target.executemany(statement, batch)
        count += len(batch)
    return count


def totals(connection):
    tokens = "+".join(TOKEN_COLUMNS)
    return dict(zip(
        ("sessions", "events", "recordedTokens"),
        connection.execute(
            f"SELECT COUNT(*),COALESCE(SUM(event_count),0),COALESCE(SUM({tokens}),0) "
            "FROM session_stats"
        ).fetchone(),
    ))


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        while block := stream.read(1 << 20):
            h.update(block)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    args = parser.parse_args()
    source_path = args.source.resolve(strict=True)
    destination = args.destination.resolve()
    if destination.exists() or destination.with_suffix(destination.suffix + ".partial").exists():
        raise FileExistsError("statistics destination or partial file already exists")
    if source_path == destination:
        raise ValueError("source and destination must differ")
    destination.parent.mkdir(parents=True, exist_ok=True)
    partial = destination.with_suffix(destination.suffix + ".partial")
    source = sqlite3.connect(source_path.as_uri() + "?mode=ro", uri=True, timeout=5)
    target = sqlite3.connect(partial)
    try:
        source.execute("PRAGMA query_only=ON")
        source.execute("BEGIN")
        target.execute("PRAGMA journal_mode=DELETE")
        target.execute("PRAGMA synchronous=FULL")
        target.execute("BEGIN")
        counts = {"session_stats": create_and_copy(source, target, "query_sessions", "session_stats", SESSION_COLUMNS)}
        for table, fields in STAT_TABLES.items():
            counts[table] = create_and_copy(source, target, table, table, fields)
        target.execute("CREATE UNIQUE INDEX session_stats_id ON session_stats(id)")
        target.execute("CREATE INDEX session_stats_activity ON session_stats(last_activity DESC,id)")
        target.execute("CREATE INDEX session_stats_machine ON session_stats(machine_id,last_activity DESC,id)")
        target.execute("CREATE INDEX agent_usage_session ON query_agent_usage(session_id)")
        target.execute("CREATE INDEX model_usage_session ON query_model_usage(session_id)")
        target.execute("CREATE INDEX daily_usage_session ON query_daily_usage(session_id)")
        target.execute("CREATE INDEX event_agent_session ON query_event_agents(session_id)")
        target.execute("CREATE INDEX flow_tool_session ON query_flow_tools_v1(session_id)")
        actual = totals(target)
        expected = source.execute(
            "SELECT COUNT(*),COALESCE(SUM(event_count),0),"
            "COALESCE(SUM(tokens_in+tokens_cache+tokens_write+tokens_out),0) "
            "FROM query_sessions"
        ).fetchone()
        if tuple(actual.values()) != expected:
            raise RuntimeError(f"statistics mismatch: {actual} != {expected}")
        target.commit()
        source.rollback()
    finally:
        target.close()
        source.close()
    verify = sqlite3.connect(partial)
    try:
        if verify.execute("PRAGMA quick_check").fetchone()[0] != "ok":
            raise RuntimeError("statistics snapshot failed quick_check")
        if totals(verify) != actual:
            raise RuntimeError("statistics changed after reopening")
        for table, expected_count in counts.items():
            observed_count = verify.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
            if observed_count != expected_count:
                raise RuntimeError(f"{table} changed after reopening: {observed_count} != {expected_count}")
        forbidden = {"events", "events_fts", "chunks", "sources", "usage_observations", "legacy_envelopes"}
        saved = {row[0] for row in verify.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        if forbidden & saved:
            raise RuntimeError("statistics snapshot contains raw-history tables")
    finally:
        verify.close()
    partial.replace(destination)
    manifest = {
        "createdAt": datetime.now(timezone.utc).isoformat(),
        "scope": "per-conversation aggregates only; no message text or raw transcripts",
        "source": str(source_path),
        "destination": str(destination),
        "tables": counts,
        "totals": actual,
        "bytes": destination.stat().st_size,
        "sha256": digest(destination),
        "liveHubChanged": False,
    }
    destination.with_suffix(destination.suffix + ".json").write_text(
        json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(manifest, indent=2))


if __name__ == "__main__":
    main()
