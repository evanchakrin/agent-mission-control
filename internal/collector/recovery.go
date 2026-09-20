package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func (c *Collector) recoveryLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.recover:
			for {
				var epoch string
				if err := c.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='recovery_pending'").Scan(&epoch); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						break
					}
					return fmt.Errorf("read durable recovery intent: %w", err)
				}
				err := c.ReconcileRemote(ctx)
				if err == nil {
					_, err = c.db.ExecContext(ctx, "DELETE FROM settings WHERE key='recovery_pending' AND value=?", epoch)
					if err != nil {
						return err
					}
					c.wakeUploads()
					break
				}
				c.setConnection("reconciling", err)
				if !wait(ctx, 30*time.Second) {
					return ctx.Err()
				}
			}
		}
	}
}

// ReconcileRemote checks committed offsets after a hub restore. Queries and
// requests use small pages so recovery memory is independent of catalog size.
func (c *Collector) ReconcileRemote(ctx context.Context) error {
	var sourceCursor, generationCursor string
	for {
		rows, err := c.db.QueryContext(ctx, `SELECT g.source_id,g.generation,s.provider,s.native_id,g.path,g.size,g.modified_ns,g.generation_sequence
		 FROM generations g JOIN sources s ON s.id=g.source_id WHERE (g.source_id,g.generation)>(?,?)
		 ORDER BY g.source_id,g.generation LIMIT 64`, sourceCursor, generationCursor)
		if err != nil {
			return err
		}
		var page []protocol.Source
		for rows.Next() {
			var s protocol.Source
			var modified int64
			if err = rows.Scan(&s.SourceID, &s.Generation, &s.Provider, &s.NativeID, &s.Path, &s.Size, &modified, &s.GenerationSequence); err != nil {
				rows.Close()
				return err
			}
			s.MachineID = c.cfg.MachineID
			s.ModifiedAt = time.Unix(0, modified).UTC()
			page = append(page, s)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, s := range page {
			if err = c.reconcileSource(ctx, s); err != nil {
				return err
			}
			sourceCursor, generationCursor = s.SourceID, s.Generation
		}
	}
}

func (c *Collector) reconcileSource(ctx context.Context, s protocol.Source) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	body, _ := json.Marshal(s)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.HubURL, "/")+"/v2/ingest/reconcile", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	var receipt protocol.Receipt
	if err = decodeReceipt(resp.Body, &receipt); err != nil {
		return err
	}
	if receipt.SourceID != s.SourceID || receipt.Generation != s.Generation || receipt.DurableOffset < 0 || receipt.DurableOffset > s.Size {
		return errors.New("invalid reconciliation receipt")
	}
	// The upload lease lock prevents a concurrent lease from observing a
	// half-updated recovery cursor. In-flight uploads remain idempotent.
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE chunks SET ack_at=NULL,receipt_id=NULL,next_attempt=0,lease_until=0
	 WHERE source_id=? AND generation=? AND offset+length>?`, s.SourceID, s.Generation, receipt.DurableOffset); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE chunks SET ack_at=COALESCE(ack_at,?),lease_until=0
	 WHERE source_id=? AND generation=? AND offset+length<=?`, time.Now().UnixNano(), s.SourceID, s.Generation, receipt.DurableOffset); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE generations SET uploaded=min(captured,?),remote_checked=? WHERE source_id=? AND generation=?`, receipt.DurableOffset, time.Now().UnixNano(), s.SourceID, s.Generation); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Collector) restorePayload(ctx context.Context, r *pendingChunk) error {
	f, err := os.Open(r.chunk.Source.Path)
	if err != nil {
		if gapErr := c.recordGap(ctx, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset, r.chunk.Length, "hub restore requires bytes no longer available in source or spool"); gapErr != nil {
			return errors.Join(err, fmt.Errorf("persist recovery gap: %w", gapErr))
		}
		return fmt.Errorf("recovery gap: source and acknowledged spool copy unavailable: %w", err)
	}
	defer f.Close()
	if err = c.ensureSpace(ctx, r.chunk.Length); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(c.cfg.DataDir, "chunks"), "recovery-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(tmp, h), io.NewSectionReader(f, r.chunk.Offset, r.chunk.Length), make([]byte, 64<<10))
	if err != nil {
		tmp.Close()
		return err
	}
	if n != r.chunk.Length || hex.EncodeToString(h.Sum(nil)) != r.chunk.SHA256 {
		tmp.Close()
		if gapErr := c.recordGap(ctx, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset, r.chunk.Length, "hub restore requires bytes that changed after spool reclamation"); gapErr != nil {
			return fmt.Errorf("persist recovery gap for changed source: %w", gapErr)
		}
		return errors.New("recovery gap: current source does not match previously captured evidence")
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	final := filepath.Join(c.cfg.DataDir, "chunks", r.name)
	if _, err = os.Stat(final); errors.Is(err, os.ErrNotExist) {
		if err = durableRename(tmp.Name(), final); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err = verifyPayload(final, r.chunk.Length, r.chunk.SHA256); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE chunks SET payload_present=1 WHERE source_id=? AND generation=? AND offset=?`, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset)
	return err
}
