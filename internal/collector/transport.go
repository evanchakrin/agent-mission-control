package collector

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type pendingChunk struct {
	chunk    protocol.Chunk
	name     string
	attempts int
	present  bool
}
type transferError struct {
	code       string
	retryAfter time.Duration
	status     int
}

func (e *transferError) Error() string { return fmt.Sprintf("hub %s (HTTP %d)", e.code, e.status) }

const leaseSelectQuery = `SELECT k.source_id,k.generation,k.offset,k.length,k.sha256,k.filename,k.attempts,k.payload_present,
 s.provider,s.native_id,g.path,g.size,g.modified_ns,g.generation_sequence FROM upload_sources q INDEXED BY upload_sources_order
 CROSS JOIN sources s ON s.id=q.source_id
 CROSS JOIN chunks k INDEXED BY pending_source_chunks ON k.source_id=q.source_id
 CROSS JOIN generations g ON g.source_id=k.source_id AND g.generation=k.generation
 WHERE k.ack_at IS NULL AND k.next_attempt<=? AND k.lease_until<=?
 AND NOT EXISTS(SELECT 1 FROM chunks prior INDEXED BY pending_source_chunks WHERE prior.source_id=k.source_id AND prior.generation=k.generation
 AND prior.offset<k.offset AND prior.ack_at IS NULL)
 ORDER BY q.last_attempt,q.source_id,k.last_attempt,k.offset LIMIT 1`

func (c *Collector) leaseChunk(ctx context.Context) (*pendingChunk, error) {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	now := time.Now().UnixNano()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	blocked, err := readUploadPause(ctx, tx)
	if err != nil {
		return nil, err
	}
	if blocked.Until > now {
		return nil, nil
	}
	var r pendingChunk
	var modified int64
	var present int
	err = tx.QueryRowContext(ctx, leaseSelectQuery, now, now).Scan(&r.chunk.Source.SourceID, &r.chunk.Source.Generation, &r.chunk.Offset, &r.chunk.Length, &r.chunk.SHA256, &r.name, &r.attempts, &present, &r.chunk.Source.Provider, &r.chunk.Source.NativeID, &r.chunk.Source.Path, &r.chunk.Source.Size, &modified, &r.chunk.Source.GenerationSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.chunk.Source.MachineID = c.cfg.MachineID
	r.chunk.Source.ModifiedAt = time.Unix(0, modified).UTC()
	r.present = present != 0
	if _, err = tx.ExecContext(ctx, `UPDATE chunks SET lease_until=?,last_attempt=? WHERE source_id=? AND generation=? AND offset=?`, time.Now().Add(c.cfg.RequestTimeout+time.Minute).UnixNano(), now, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sources SET last_upload_attempt=? WHERE id=?", now, r.chunk.Source.SourceID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}

// UploadOnce uploads at most one leased chunk. Concurrent callers still upload
// each generation in order, with distinct generations selected fairly.
func (c *Collector) UploadOnce(ctx context.Context) (bool, error) {
	r, err := c.leaseChunk(ctx)
	if err != nil || r == nil {
		return false, err
	}
	err = c.upload(ctx, r)
	if err != nil {
		delay := retryDelay(r.attempts)
		pauseState := ""
		var remote *transferError
		if errors.As(err, &remote) {
			delay = max(delay, remote.retryAfter)
			switch remote.status {
			case 401, 403:
				pauseState = "blocked_auth"
				delay = max(delay, 5*time.Minute)
				c.setConnection("blocked_auth", err)
			case 507, 413:
				pauseState = "blocked_storage"
				delay = max(delay, 5*time.Minute)
				c.setConnection("blocked_storage", err)
			case 409:
				delay = max(delay, 5*time.Minute)
				c.setConnection("blocked_conflict", err)
			default:
				if remote.status == 408 || remote.status == 429 || remote.status >= 500 {
					pauseState = "retrying"
				}
				c.setConnection("retrying", err)
			}
		} else {
			var network net.Error
			if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) {
				pauseState = "retrying"
			}
			c.setConnection("retrying", err)
		}
		// Persist backoff, including across collector restarts. Cancellation
		// releases only the lease; it never acknowledges or discards a chunk.
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Commit the chunk retry and hub-wide pause together. At most the other
		// already leased worker can still reach the hub after a blocked response.
		tx, dbErr := c.db.BeginTx(updateCtx, nil)
		if dbErr != nil {
			return true, dbErr
		}
		defer tx.Rollback()
		failures := 0
		if pauseState == "retrying" {
			previous, readErr := readUploadPause(updateCtx, tx)
			if readErr != nil {
				return true, readErr
			}
			failures = min(max(previous.Failures, 0), 9)
			delay = max(delay, retryDelay(failures))
			failures++
		}
		nextAttempt := time.Now().Add(delay).UnixNano()
		_, dbErr = tx.ExecContext(updateCtx, `UPDATE chunks SET attempts=attempts+1,next_attempt=?,lease_until=0 WHERE source_id=? AND generation=? AND offset=?`, nextAttempt, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset)
		if dbErr == nil && pauseState != "" {
			dbErr = writeUploadPause(updateCtx, tx, uploadPause{Until: nextAttempt, State: pauseState, Failures: failures})
		}
		if dbErr == nil {
			dbErr = tx.Commit()
		}
		if dbErr != nil {
			return true, dbErr
		}
		return true, err
	}
	return true, nil
}

func (c *Collector) upload(ctx context.Context, r *pendingChunk) error {
	if !r.present {
		if err := c.restorePayload(ctx, r); err != nil {
			return err
		}
	}
	path := filepath.Join(c.cfg.DataDir, "chunks", r.name)
	if err := verifyPayload(path, r.chunk.Length, r.chunk.SHA256); err != nil {
		return err
	}
	if !c.throttle(ctx, r.chunk.Length) {
		return ctx.Err()
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	progress := newProgressReader(f, c.cfg.ProgressTimeout, cancel)
	defer progress.Close()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(c.cfg.HubURL, "/")+"/v2/ingest/chunks", progress)
	if err != nil {
		return err
	}
	header, err := json.Marshal(r.chunk)
	if err != nil {
		return err
	}
	if len(header) > 12<<10 {
		return errors.New("source metadata exceeds bounded transport header")
	}
	req.Header.Set("X-AMC-Chunk", base64.RawURLEncoding.EncodeToString(header))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = r.chunk.Length
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return responseError(resp)
	}
	var receipt protocol.Receipt
	if err = decodeReceipt(resp.Body, &receipt); err != nil {
		return err
	}
	if receipt.SourceID != r.chunk.Source.SourceID || receipt.Generation != r.chunk.Source.Generation || receipt.DurableOffset < r.chunk.Offset+r.chunk.Length {
		return errors.New("hub receipt does not acknowledge the uploaded chunk")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE chunks SET ack_at=?,receipt_id=?,lease_until=0,attempts=0 WHERE source_id=? AND generation=? AND offset=?`, time.Now().UnixNano(), receipt.ReceiptID, r.chunk.Source.SourceID, r.chunk.Source.Generation, r.chunk.Offset); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE generations SET uploaded=max(uploaded,?) WHERE source_id=? AND generation=?`, r.chunk.Offset+r.chunk.Length, r.chunk.Source.SourceID, r.chunk.Source.Generation); err != nil {
		return err
	}
	// A successful probe resets the outage streak. An already in-flight success
	// must not clear a newer active pause established by the other worker.
	pause, err := readUploadPause(ctx, tx)
	if err != nil {
		return err
	}
	if pause.State == "retrying" && pause.Until <= time.Now().UnixNano() {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key='upload_pause'"); err != nil {
			return err
		}
	}
	changedEpoch, err := noteEpochTx(ctx, tx, receipt.RecoveryEpoch)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changedEpoch {
		c.wakeRecovery()
	}
	now := time.Now().UTC()
	c.mu.Lock()
	c.state.LastUploadAt = &now
	c.state.ConnectionState = "connected"
	c.state.ConnectionError = ""
	c.mu.Unlock()
	return nil
}

func retryDelay(attempt int) time.Duration {
	shift := min(attempt, 9)
	maximum := min(5*time.Minute, time.Second*time.Duration(1<<shift))
	return maximum/2 + time.Duration(rand.Int64N(int64(maximum/2)+1))
}
func (c *Collector) authorize(req *http.Request) {
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
}
func responseError(resp *http.Response) error {
	code := http.StatusText(resp.StatusCode)
	var api protocol.APIError
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&api) == nil && api.Code != "" {
		code = api.Code
	}
	// Do not copy arbitrary server response bodies into diagnostics; they can
	// contain proxy credentials or transcript text.
	var delay time.Duration
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		delay = time.Duration(min(n, 86400)) * time.Second
	} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		delay = min(max(time.Until(date), 0), 24*time.Hour)
	}
	return &transferError{code: code, status: resp.StatusCode, retryAfter: delay}
}

func (c *Collector) throttle(ctx context.Context, n int64) bool {
	c.rateMu.Lock()
	now := time.Now()
	start := c.nextSend
	if start.Before(now) {
		start = now
	}
	c.nextSend = start.Add(time.Duration(float64(n) / float64(c.cfg.BytesPerSecond) * float64(time.Second)))
	c.rateMu.Unlock()
	return wait(ctx, time.Until(start))
}

func (c *Collector) uploadLoop(ctx context.Context, signal <-chan struct{}) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		worked, err := c.UploadOnce(ctx)
		if err != nil && !wait(ctx, time.Second) {
			return ctx.Err()
		}
		if !worked && !waitForWork(ctx, signal, 5*time.Second) {
			return ctx.Err()
		}
	}
}

func waitForWork(ctx context.Context, signal <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-signal:
		return true
	case <-timer.C:
		return true
	}
}

func (c *Collector) heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := c.sendHeartbeat(ctx); err != nil {
			c.setConnection("retrying", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Collector) sendHeartbeat(ctx context.Context) error {
	// Heartbeats have their own goroutine and deadline and never wait for a
	// transcript transfer. SQLite transactions contain no network operations.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := c.Status(ctx)
	if err != nil {
		return err
	}
	hb := protocol.Heartbeat{MachineID: c.cfg.MachineID, Name: c.cfg.Name, Version: c.cfg.Version, BootID: c.bootID, UptimeSeconds: int64(time.Since(c.started) / time.Second), CapturedBytes: s.CapturedBytes, UploadedBytes: s.UploadedBytes, BacklogBytes: s.BacklogBytes, LastUploadAt: s.LastUploadAt, State: s.CollectionState, Error: s.Error}
	hb.CollectionState, hb.ConnectionState = s.CollectionState, s.ConnectionState
	c.mu.Lock()
	hb.Network = c.networkReport
	c.mu.Unlock()
	hb.HistoryGaps = &s.Gaps
	hb.CollectionError, hb.ConnectionError = s.CollectionError, s.ConnectionError
	hb.UploadRetryAt = s.UploadRetryAt
	if s.UploadRetryAt != nil && s.CollectionState == "ready" {
		hb.State = s.ConnectionState
	}
	body, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.HubURL, "/")+"/v2/ingest/heartbeat", bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	if err = c.noteEpoch(ctx, resp.Header.Get("X-AMC-Recovery-Epoch")); err != nil {
		return err
	}
	now := time.Now().UTC()
	c.mu.Lock()
	c.state.LastHeartbeatAt = &now
	if c.state.ConnectionState == "connecting" || c.state.ConnectionState == "retrying" {
		c.state.ConnectionState = "connected"
		c.state.ConnectionError = ""
	}
	c.mu.Unlock()
	return nil
}

func (c *Collector) noteEpoch(ctx context.Context, epoch string) error {
	if epoch == "" {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := noteEpochTx(ctx, tx, epoch)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed {
		c.wakeRecovery()
	}
	return nil
}

// Persist recovery intent atomically with the receipt that revealed it. A
// storage failure must not leave an acknowledged chunk without recovery intent.
func noteEpochTx(ctx context.Context, tx *sql.Tx, epoch string) (bool, error) {
	if epoch == "" {
		return false, nil
	}
	var previous string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='recovery_epoch'").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if epoch == previous {
		return false, nil
	}
	for _, key := range []string{"recovery_epoch", "recovery_pending"} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, epoch); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (c *Collector) wakeRecovery() {
	select {
	case c.recover <- struct{}{}:
	default:
	}
}

// progressReader bounds a stalled request after its last body progress. The
// absolute request deadline remains independent and always applies.
type progressReader struct {
	reader  io.ReadCloser
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	closed  bool
}

func newProgressReader(r io.ReadCloser, d time.Duration, cancel context.CancelFunc) *progressReader {
	return &progressReader{reader: r, timer: time.AfterFunc(d, cancel), timeout: d}
}
func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.mu.Lock()
		if !r.closed {
			r.timer.Reset(r.timeout)
		}
		r.mu.Unlock()
	}
	return n, err
}
func (r *progressReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.timer.Stop()
	r.mu.Unlock()
	return r.reader.Close()
}
