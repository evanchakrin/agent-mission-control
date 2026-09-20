"""Ensure every not-yet-indexed compact-ledger receipt has its source chunk."""

import argparse
import hashlib
import os
import shutil
import sqlite3
from pathlib import Path


def hash_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while part := stream.read(1 << 20):
            digest.update(part)
    return digest.hexdigest()


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--candidate", type=Path, required=True)
parser.add_argument("--old-hub", type=Path, required=True)
args = parser.parse_args()
candidate = args.candidate.resolve(strict=True)
old_hub = args.old_hub.resolve(strict=True)
db = sqlite3.connect(candidate.as_uri() + "?mode=ro", uri=True)
try:
    pending = list(db.execute(
        "SELECT c.sha256,MAX(c.length) FROM chunks c JOIN sources s USING(source_id,generation) "
        "WHERE c.offset+c.length>s.indexed_offset GROUP BY c.sha256"
    ))
finally:
    db.close()
copied = 0
for hash_value, length in pending:
    new_path = candidate.parent / "blobs" / hash_value[:2] / hash_value
    if not new_path.exists():
        old_path = old_hub / "blobs" / hash_value[:2] / hash_value
        new_path.parent.mkdir(parents=True, exist_ok=True)
        with old_path.open("rb") as incoming, new_path.open("xb") as outgoing:
            shutil.copyfileobj(incoming, outgoing, 1 << 20)
            outgoing.flush()
            os.fsync(outgoing.fileno())
        copied += 1
    if new_path.stat().st_size != length or hash_file(new_path) != hash_value:
        raise RuntimeError(f"pending chunk invalid: {hash_value}")
print(f"verified {len(pending)} pending unique chunks; copied {copied} missing chunks")
