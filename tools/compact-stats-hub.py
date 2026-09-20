"""Build a separate live-compatible AMC ledger without old transcript content.

The source is read-only. The destination must not exist. This is preparation,
not a cutover or permission to delete the source; the hub must be stopped and
the result rechecked against the final source state before use.
"""

import argparse
import hashlib
import json
import os
import shutil
import sqlite3
from pathlib import Path


# Everything outside this list is raw history, transcript-derived evidence, an
# old task queue, or a rebuildable text index. In particular, events and FTS
# are deliberately empty in the new ledger.
KEEP = {
    "properties", "source_identity", "sources", "chunks", "sessions",
    "usage_observations", "session_metadata", "agent_names",
    "metadata_operations", "projects", "project_operations", "project_deletions",
    "project_audit", "organization_audit", "legacy_aliases", "machines",
    "machine_labels", "machine_label_operations", "machine_label_audit",
    "query_sessions", "query_usage", "query_agent_usage", "query_model_usage",
    "query_daily_usage", "query_event_agents", "query_flow_tools_v1",
    "current_comparisons", "ledger_totals", "rate_catalogs", "pricing_policies",
    "pricing_policy_comparisons", "pricing_comparison_default",
    "pricing_checkpoints", "accounting_estimates", "observation_prices",
    "observation_price_versions", "economics_history",
    "economics_capture_resolutions", "usage_contribution_owners",
    "usage_contribution_selections", "active_projection", "projection_revisions",
    "baseline_projections", "reclaimed_blobs",
}
TEXT_INDEXES = {"events_fts", "catalog_search"}
JSON_REDACT = {
    "sources": {"parser_state": ("title",)},
    "sessions": {"projection": ("title",)},
    "query_sessions": {},
    "usage_observations": {"observation": ("evidence",)},
    "projection_revisions": {"parser_state": ("title",), "projection": ("title",)},
    "baseline_projections": {"parser_state": ("title",), "projection": ("title",)},
}


def redact_json(value, names):
    if value is None:
        return value
    obj = json.loads(value)
    if isinstance(obj, dict):
        for name in names:
            obj.pop(name, None)
    result = json.dumps(obj, separators=(",", ":"))
    return result.encode() if isinstance(value, bytes) else result


def copy_rows(source, target, table):
    description = source.execute(f'SELECT * FROM "{table}" LIMIT 0').description
    fields = [entry[0] for entry in description]
    sql = f'INSERT INTO "{table}" VALUES ({",".join("?" for _ in fields)})'
    edits = JSON_REDACT.get(table, {})
    positions = {fields.index(name): names for name, names in edits.items() if name in fields}
    title_pos = fields.index("title") if table == "query_sessions" else None
    cursor = source.execute(f'SELECT * FROM "{table}"')
    count = 0
    while batch := cursor.fetchmany(500):
        prepared = []
        for item in batch:
            row = list(item)
            for index, names in positions.items():
                row[index] = redact_json(row[index], names)
            if title_pos is not None:
                row[title_pos] = ""
            prepared.append(row)
        target.executemany(sql, prepared)
        count += len(batch)
    return count


def totals(db):
    return db.execute("SELECT COUNT(*),COALESCE(SUM(event_count),0),"
                      "COALESCE(SUM(tokens_in+tokens_cache+tokens_write+tokens_out),0)"
                      " FROM query_sessions").fetchone()


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        while part := stream.read(1 << 20):
            h.update(part)
    return h.hexdigest()


def copy_pending_blobs(source, old_root, new_root):
    pending = source.execute(
        "SELECT c.sha256,MAX(c.length) FROM chunks c JOIN sources s USING(source_id,generation) "
        "WHERE c.offset+c.length>s.indexed_offset GROUP BY c.sha256"
    )
    copied = 0
    for hash_value, length in pending:
        old_file = old_root / "blobs" / hash_value[:2] / hash_value
        new_dir = new_root / "blobs" / hash_value[:2]
        new_dir.mkdir(parents=True, exist_ok=True)
        new_file = new_dir / hash_value
        if new_file.exists():
            raise FileExistsError(new_file)
        with old_file.open("rb") as incoming, new_file.open("xb") as outgoing:
            shutil.copyfileobj(incoming, outgoing, 1 << 20)
            outgoing.flush()
            os.fsync(outgoing.fileno())
        if new_file.stat().st_size != length or digest(new_file) != hash_value:
            raise RuntimeError(f"pending chunk integrity failure: {hash_value}")
        copied += length
    return copied


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    args = parser.parse_args()
    src = args.source.resolve(strict=True)
    dst = args.destination.resolve()
    partial = dst.with_suffix(dst.suffix + ".partial")
    if dst.exists() or partial.exists() or src == dst:
        raise ValueError("destination must be new and distinct from source")
    if (dst.parent / "blobs").exists():
        raise FileExistsError("destination already has a blobs directory")
    dst.parent.mkdir(parents=True, exist_ok=True)
    source = sqlite3.connect(src.as_uri() + "?mode=ro", uri=True, timeout=30)
    target = sqlite3.connect(partial)
    try:
        source.execute("PRAGMA query_only=ON")
        source.execute("BEGIN")
        schema = list(source.execute("SELECT type,name,tbl_name,sql FROM sqlite_master "
                                     "WHERE sql IS NOT NULL ORDER BY rowid"))
        tables = {name: sql for kind, name, _, sql in schema if kind == "table"}
        missing = KEEP - set(tables) - {"reclaimed_blobs"}
        if missing:
            raise ValueError(f"required tables absent: {sorted(missing)}")
        target.execute("PRAGMA journal_mode=DELETE")
        target.execute("PRAGMA synchronous=FULL")
        target.execute("PRAGMA foreign_keys=OFF")
        target.execute("BEGIN")
        for kind, name, _, sql in schema:
            if kind == "table" and not name.startswith("sqlite_") and (
                name in KEEP or name in {"events", *TEXT_INDEXES}
            ):
                target.execute(sql)
        if "reclaimed_blobs" not in tables:
            target.execute("CREATE TABLE reclaimed_blobs(hash TEXT PRIMARY KEY)")
        counts = {}
        for kind, name, _, _ in schema:
            if kind == "table" and name in KEEP and name in tables:
                counts[name] = copy_rows(source, target, name)
        target.execute("INSERT INTO properties(key,value) VALUES('storage_mode','stats-only') "
                       "ON CONFLICT(key) DO UPDATE SET value=excluded.value")
        target.execute("DELETE FROM properties WHERE key='catalog_search_schema'")
        # The old receipts are needed; their raw blobs are intentionally not.
        target.execute("INSERT OR IGNORE INTO reclaimed_blobs(hash) "
                       "SELECT c.sha256 FROM chunks c JOIN sources s USING(source_id,generation) "
                       "GROUP BY c.sha256 HAVING SUM(c.offset+c.length>s.indexed_offset)=0")
        # Index definitions must follow data loading so text-derived triggers do
        # not copy or reconstruct the old conversation corpus.
        for kind, name, table, sql in schema:
            if kind == "index" and table in KEEP | {"events", *TEXT_INDEXES}:
                target.execute(sql)
        for kind, name, table, sql in schema:
            if kind == "trigger" and table in KEEP | {"events", *TEXT_INDEXES}:
                target.execute(sql)
        target.execute(f"PRAGMA user_version={source.execute('PRAGMA user_version').fetchone()[0]}")
        expected = totals(source)
        actual = totals(target)
        if actual != expected:
            raise RuntimeError(f"statistics mismatch: {actual} != {expected}")
        if target.execute("SELECT COUNT(*) FROM events").fetchone()[0] != 0:
            raise RuntimeError("new ledger unexpectedly contains events")
        target.commit()
        pending_raw_bytes = copy_pending_blobs(source, src.parent, dst.parent)
        source.rollback()
    finally:
        target.close()
        source.close()
    check = sqlite3.connect(partial)
    try:
        if check.execute("PRAGMA quick_check").fetchone()[0] != "ok":
            raise RuntimeError("compact ledger failed quick_check")
        if totals(check) != actual:
            raise RuntimeError("compact ledger changed on reopen")
        for table, count in counts.items():
            if check.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != count:
                raise RuntimeError(f"{table} count changed on reopen")
    finally:
        check.close()
    partial.replace(dst)
    print(json.dumps({"destination": str(dst), "bytes": dst.stat().st_size,
                      "sha256": digest(dst), "totals": actual,
                      "pendingRawBytes": pending_raw_bytes, "tables": counts}, indent=2))


if __name__ == "__main__":
    main()
