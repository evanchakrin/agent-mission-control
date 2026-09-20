package collector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

var ErrSpoolFull = errors.New("collector spool is full; unacknowledged history is retained and capture is paused")
var ErrDiskReserve = errors.New("collector storage reserve reached; capture is paused without discarding history")

func (c *Collector) reclaimLoop(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		if err := c.reclaim(ctx, false, 0); err != nil {
			c.setCollection("blocked_storage", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// CaptureOnce captures at most one chunk. Last-capture ordering rotates fairly
// across the catalog instead of draining a large transcript before its peers.
func (c *Collector) CaptureOnce(ctx context.Context) error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	var id, gen, path string
	var size, offset int64
	err := c.db.QueryRowContext(ctx, `SELECT g.source_id,g.generation,s.path,g.size,g.captured FROM generations g
	 JOIN sources s ON s.id=g.source_id AND s.current_generation=g.generation
	 WHERE s.missing=0 AND g.captured<g.size ORDER BY g.last_capture,g.source_id LIMIT 1`).Scan(&id, &gen, &path, &size, &offset)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	length := min(int64(protocol.MaxChunkBytes), size-offset)
	if err = c.ensureSpace(ctx, length); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if gapErr := c.markSourceMissing(ctx, id, gen, offset, size-offset); gapErr != nil {
				return errors.Join(err, gapErr)
			}
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < offset+length {
		c.wake()
		return errors.New("source changed during capture; awaiting catalog reconciliation")
	}
	// Verify the previous chunk immediately before appending evidence. Full
	// catalog verification catches non-boundary rewrites independently.
	if offset > 0 {
		ok, err := c.verifyCaptured(ctx, f, id, gen, offset, false)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("source rewrite detected; awaiting catalog reconciliation")
		}
	}
	tmp, err := os.CreateTemp(filepath.Join(c.cfg.DataDir, "chunks"), "capture-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	h := sha256.New()
	n, copyErr := io.CopyBuffer(io.MultiWriter(tmp, h), io.NewSectionReader(f, offset, length), make([]byte, 64<<10))
	if copyErr != nil {
		tmp.Close()
		return copyErr
	}
	if n != length {
		tmp.Close()
		return io.ErrUnexpectedEOF
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	key := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s", id, gen, offset, digest)))
	name := hex.EncodeToString(key[:]) + ".chunk"
	final := filepath.Join(c.cfg.DataDir, "chunks", name)
	// A crash after file rename and before the DB transaction leaves an orphan
	// with the same content-addressed name. Reusing it is safe only after hash
	// verification; conflicting bytes can never overwrite accepted evidence.
	if _, statErr := os.Stat(final); statErr == nil {
		if err = verifyPayload(final, length, digest); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if err = durableRename(tmpPath, final); err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO chunks(source_id,generation,offset,length,sha256,filename) VALUES(?,?,?,?,?,?)`, id, gen, offset, length, digest, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE generations SET captured=?,last_capture=? WHERE source_id=? AND generation=? AND captured=?`, offset+length, time.Now().UnixNano(), id, gen, offset); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	now := time.Now().UTC()
	c.mu.Lock()
	c.state.LastCaptureAt = &now
	c.state.CollectionState = "ready"
	c.mu.Unlock()
	c.wakeUploads()
	c.wake()
	return nil
}

func verifyPayload(path string, length int64, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(f, length+1), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if n != length || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("spool chunk failed integrity verification")
	}
	return nil
}

func (c *Collector) ensureSpace(ctx context.Context, incoming int64) error {
	if err := c.reclaim(ctx, false, incoming); err != nil {
		return err
	}
	var used int64
	if err := c.db.QueryRowContext(ctx, "SELECT spool FROM catalog_stats WHERE id=1").Scan(&used); err != nil {
		return err
	}
	free, total, err := c.freeSpace(c.cfg.DataDir)
	if err != nil {
		return err
	}
	reserve := max(uint64(5<<30), total/20)
	if used+incoming > c.cfg.SpoolMaxBytes || free < uint64(incoming)+reserve {
		if err = c.reclaim(ctx, true, incoming); err != nil {
			return err
		}
		if err = c.db.QueryRowContext(ctx, "SELECT spool FROM catalog_stats WHERE id=1").Scan(&used); err != nil {
			return err
		}
		free, total, err = c.freeSpace(c.cfg.DataDir)
		if err != nil {
			return err
		}
		reserve = max(uint64(5<<30), total/20)
	}
	if used+incoming > c.cfg.SpoolMaxBytes {
		return ErrSpoolFull
	}
	if free < uint64(incoming)+reserve {
		return ErrDiskReserve
	}
	return nil
}

func (c *Collector) reclaim(ctx context.Context, pressure bool, incoming int64) error {
	for {
		cutoff := time.Now().Add(-24 * time.Hour).UnixNano()
		if pressure {
			cutoff = time.Now().UnixNano() + 1
		}
		found, err := c.reclaimOne(ctx, cutoff)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if pressure {
			var used int64
			if err = c.db.QueryRowContext(ctx, "SELECT spool FROM catalog_stats WHERE id=1").Scan(&used); err != nil {
				return err
			}
			free, total, err := c.freeSpace(c.cfg.DataDir)
			if err != nil {
				return err
			}
			if used+incoming <= c.cfg.SpoolMaxBytes && free >= uint64(incoming)+max(uint64(5<<30), total/20) {
				return nil
			}
		}
	}
}

func (c *Collector) reclaimOne(ctx context.Context, cutoff int64) (bool, error) {
	// Reconciliation uses this same lock when revoking acknowledgements. Keep
	// selection, state transition and deletion together so a recovery lease cannot
	// see present bytes and then lose them to an earlier cleanup decision. Lock
	// only one payload at a time, not an entire potentially large reclaim pass.
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	var id, gen, name string
	var offset int64
	err := c.db.QueryRowContext(ctx, `SELECT source_id,generation,offset,filename FROM chunks WHERE payload_present=1 AND ack_at IS NOT NULL AND ack_at<? ORDER BY ack_at LIMIT 1`, cutoff).Scan(&id, &gen, &offset, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := c.db.ExecContext(ctx, `UPDATE chunks SET payload_present=0 WHERE source_id=? AND generation=? AND offset=? AND payload_present=1 AND ack_at IS NOT NULL AND ack_at<?`, id, gen, offset, cutoff)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return true, nil // Candidate no longer eligible; rescan without deleting.
	}
	if err = os.Remove(filepath.Join(c.cfg.DataDir, "chunks", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}
