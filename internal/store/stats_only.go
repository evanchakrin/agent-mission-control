package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
)

// StatsOnly is a database property, not a process flag: the hub and its parser
// child must agree on what evidence may be retained after a restart.
func (s *Store) StatsOnly() bool { return s.statsOnly }

// ReclaimIndexedBlobs removes only chunks whose source checkpoint is durable
// and whose hash has no unindexed reference. Receipts remain in the ledger for
// idempotent collector retries. A failed unlink is retried on the next pass.
func (s *Store) ReclaimIndexedBlobs(ctx context.Context, limit int) (int, error) {
	if !s.statsOnly {
		return 0, nil
	}
	if limit <= 0 || limit > 128 {
		limit = 128
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT c.sha256 FROM chunks c
	 JOIN sources s USING(source_id,generation)
	 LEFT JOIN reclaimed_blobs r ON r.hash=c.sha256
	 WHERE r.hash IS NULL AND c.offset+c.length<=s.indexed_offset
	 AND NOT EXISTS (SELECT 1 FROM chunks pending JOIN sources ps USING(source_id,generation)
	                 WHERE pending.sha256=c.sha256 AND pending.offset+pending.length>ps.indexed_offset)
	 LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, hash := range hashes {
		if err = s.blobMu.LockContext(ctx); err != nil {
			return removed, err
		}
		if s.pendingBlobs[hash] != 0 {
			s.blobMu.Unlock()
			continue
		}
		if err = s.writeMu.LockContext(ctx); err != nil {
			s.blobMu.Unlock()
			return removed, err
		}
		var pending int
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chunks c JOIN sources s USING(source_id,generation)
		 WHERE c.sha256=? AND c.offset+c.length>s.indexed_offset)`, hash).Scan(&pending)
		if err == nil && pending == 0 {
			err = os.Remove(s.blobPath(hash))
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err == nil {
				_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reclaimed_blobs(hash) VALUES(?)`, hash)
				if err == nil {
					removed++
				}
			}
		}
		s.writeMu.Unlock()
		s.blobMu.Unlock()
		if err != nil && err != sql.ErrNoRows {
			return removed, err
		}
	}
	return removed, nil
}
