package collector

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reconcile discovers the complete configured catalog, including archived
// Codex trees when their explicit parent root is configured. Missing roots do
// not erase catalog rows, captured bytes, or pending transfers.
func (c *Collector) Reconcile(ctx context.Context) error { return c.reconcile(ctx, nil) }

func (c *Collector) reconcile(ctx context.Context, watcher *sourceWatcher) error {
	stamp := time.Now().UnixNano()
	var firstErr error
	for _, root := range c.cfg.Roots {
		err := walkBounded(root.Path, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("source discovery: %w", walkErr)
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if watcher != nil {
					if err := watcher.Add(path); err != nil && firstErr == nil {
						firstErr = fmt.Errorf("watcher unavailable: %w", err)
					}
				}
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
				return nil
			}
			if err := c.discoverFile(ctx, root.Provider, path, stamp, true); err != nil && firstErr == nil {
				firstErr = err
			}
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Stat each unseen path before marking it missing: an inaccessible root is
	// blocked, not evidence that its transcripts were deleted.
	var cursor string
	for {
		rows, err := c.db.QueryContext(ctx, `SELECT s.id,s.path,s.current_generation,g.captured,g.size FROM sources s
		 JOIN generations g ON g.source_id=s.id AND g.generation=s.current_generation
		 WHERE s.last_seen<? AND s.id>? ORDER BY s.id LIMIT 128`, stamp, cursor)
		if err != nil {
			return err
		}
		type missing struct {
			id, path, gen  string
			captured, size int64
		}
		var page []missing
		for rows.Next() {
			var r missing
			if err = rows.Scan(&r.id, &r.path, &r.gen, &r.captured, &r.size); err != nil {
				rows.Close()
				return err
			}
			page = append(page, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			cursor = r.id
			_, statErr := os.Stat(r.path)
			if !errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if err = c.markSourceMissing(ctx, r.id, r.gen, r.captured, r.size-r.captured); err != nil {
				return err
			}
		}
	}
	if firstErr != nil {
		c.setCollection("blocked_source", firstErr)
		return firstErr
	}
	c.setCollection("ready", nil)
	c.wake()
	return nil
}

func (c *Collector) discoverFile(ctx context.Context, provider, path string, stamp int64, fullVerification bool) error {
	// Serialize generation switches with capture so a rewrite cannot advance
	// the previous generation's checkpoint after a new generation is selected.
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	identity, err := fileIdentity(f)
	if err != nil {
		return err
	}
	identity = provider + ":" + identity
	var id, gen, previousPath, native string
	var missing int
	var size, modified, captured, verified int64
	replaced := false
	err = c.db.QueryRowContext(ctx, `SELECT s.id,s.current_generation,g.size,g.modified_ns,g.captured,g.verified_ns,s.path,s.native_id,s.missing
	 FROM sources s JOIN generations g ON g.source_id=s.id AND g.generation=s.current_generation WHERE s.identity=?`, identity).Scan(&id, &gen, &size, &modified, &captured, &verified, &previousPath, &native, &missing)
	if errors.Is(err, sql.ErrNoRows) {
		// Atomic rewrites change OS file identity. Only a unique same-path,
		// same-provider record with explicit matching native identity can be
		// continued. Ambiguous paths and different chats remain separate.
		if observed := nativeSessionID(f); observed != "" {
			err = c.db.QueryRowContext(ctx, `SELECT s.id,s.current_generation,g.size,g.modified_ns,g.captured,g.verified_ns,s.path,s.native_id,s.missing
			 FROM sources s JOIN generations g ON g.source_id=s.id AND g.generation=s.current_generation
			 WHERE s.path=? AND s.provider=? AND s.native_id=?
			 AND (SELECT count(*) FROM sources WHERE path=? AND provider=?)=1`, path, provider, observed, path, provider).Scan(&id, &gen, &size, &modified, &captured, &verified, &previousPath, &native, &missing)
			replaced = err == nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		id, gen = identifier(), identifier()
		native := nativeSessionID(f)
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO sources(id,identity,path,provider,native_id,current_generation,last_seen) VALUES(?,?,?,?,?,?,?)`, id, identity, path, provider, native, gen, stamp); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO generations(source_id,generation,path,size,modified_ns,verified_ns) VALUES(?,?,?,?,?,?)`, id, gen, path, info.Size(), info.ModTime().UnixNano(), info.ModTime().UnixNano()); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	changed := modified != info.ModTime().UnixNano() || size != info.Size()
	if !replaced && !changed && path == previousPath && missing == 0 && (!fullVerification || verified == modified) {
		return nil
	}
	if native == "" {
		native = nativeSessionID(f)
	}
	rewrite := replaced || info.Size() < captured
	if !rewrite && captured > 0 && (changed || (fullVerification && verified != modified)) {
		// Notifications use boundary verification for append workloads. Full
		// reconciliation verifies all accepted ranges whenever a source changed
		// since its last full check, detecting same-size and middle rewrites.
		all := fullVerification || info.Size() <= size
		ok, checkErr := c.verifyCaptured(ctx, f, id, gen, captured, all)
		if checkErr != nil {
			return checkErr
		}
		rewrite = !ok
		if all {
			verified = info.ModTime().UnixNano()
		}
	}
	if rewrite {
		if size > captured {
			if err = c.recordGap(ctx, id, gen, captured, size-captured, "source rewritten before capture"); err != nil {
				return err
			}
		}
		gen = identifier()
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO generations(source_id,generation,path,size,modified_ns,verified_ns,generation_sequence) VALUES(?,?,?,?,?,?,(SELECT COALESCE(max(generation_sequence),0)+1 FROM generations WHERE source_id=?))`, id, gen, path, info.Size(), info.ModTime().UnixNano(), info.ModTime().UnixNano(), id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sources SET current_generation=?,path=?,last_seen=?,missing=0,identity=? WHERE id=?`, gen, path, stamp, identity, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sources SET path=?,native_id=?,last_seen=?,missing=0 WHERE id=?`, path, native, stamp, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE generations SET path=?,size=?,modified_ns=?,verified_ns=? WHERE source_id=? AND generation=?`, path, info.Size(), info.ModTime().UnixNano(), verified, id, gen); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Collector) verifyCaptured(ctx context.Context, f *os.File, id, gen string, captured int64, all bool) (bool, error) {
	buf := make([]byte, 64<<10)
	cursor := int64(-1)
	for {
		q := `SELECT offset,length,sha256 FROM chunks WHERE source_id=? AND generation=? AND offset>?`
		args := []any{id, gen, cursor}
		if !all {
			q += ` AND (offset=0 OR offset=(SELECT max(offset) FROM chunks WHERE source_id=? AND generation=?))`
			args = append(args, id, gen)
		}
		q += ` ORDER BY offset LIMIT 128`
		rows, err := c.db.QueryContext(ctx, q, args...)
		if err != nil {
			return false, err
		}
		type rangeHash struct {
			offset, length int64
			expected       string
		}
		var page []rangeHash
		for rows.Next() {
			var r rangeHash
			if err = rows.Scan(&r.offset, &r.length, &r.expected); err != nil {
				rows.Close()
				return false, err
			}
			page = append(page, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			return true, nil
		}
		// Never hold a database connection while hashing historical bytes:
		// heartbeat/status reads must remain independent of reconciliation.
		for _, r := range page {
			if err = ctx.Err(); err != nil {
				return false, err
			}
			hash := sha256.New()
			n, copyErr := io.CopyBuffer(hash, io.NewSectionReader(f, r.offset, r.length), buf)
			if copyErr != nil {
				return false, copyErr
			}
			if n != r.length || hex.EncodeToString(hash.Sum(nil)) != r.expected {
				return false, nil
			}
			cursor = r.offset
		}
		if !all {
			return true, nil
		}
	}
}

// filepath.WalkDir sorts a complete directory in memory. This walker reads
// bounded batches; a flat archive with 100,000 files must not inflate memory.
func walkBounded(path string, fn fs.WalkDirFunc) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fn(path, nil, err)
	}
	entry := fs.FileInfoToDirEntry(info)
	if err = fn(path, entry, nil); err != nil {
		return err
	}
	if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fn(path, entry, err)
	}
	defer f.Close()
	for {
		children, readErr := f.ReadDir(128)
		for _, child := range children {
			if err = walkBounded(filepath.Join(path, child.Name()), fn); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fn(path, entry, readErr)
		}
	}
}

func nativeSessionID(f *os.File) string {
	r := bufio.NewScanner(io.NewSectionReader(f, 0, 64<<10))
	r.Buffer(make([]byte, 4096), 64<<10)
	for r.Scan() {
		var v struct {
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Payload   struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(r.Bytes(), &v) != nil {
			continue
		}
		if v.SessionID != "" {
			return v.SessionID
		}
		if v.Type == "session_meta" && v.Payload.ID != "" {
			return v.Payload.ID
		}
	}
	return ""
}

func (c *Collector) recordGap(ctx context.Context, id, gen string, offset, length int64, reason string) error {
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO gaps(source_id,generation,offset,length,reason,created_at) VALUES(?,?,?,?,?,?)`, id, gen, offset, length, reason, time.Now().UnixNano())
	return err
}

// Retiring a missing source and recording uncaptured evidence are one durable
// decision. On failure, capture must remain eligible to retry the gap write.
func (c *Collector) markSourceMissing(ctx context.Context, id, gen string, offset, length int64) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if length > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO gaps(source_id,generation,offset,length,reason,created_at) VALUES(?,?,?,?,?,?)`, id, gen, offset, length, "source disappeared before capture", time.Now().UnixNano()); err != nil {
			return fmt.Errorf("record missing source gap: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sources SET missing=1 WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Collector) discoveryLoop(ctx context.Context) error {
	watcher, err := newSourceWatcher()
	if err != nil {
		return fmt.Errorf("start filesystem notifications: %w", err)
	}
	defer watcher.Close()
	c.refreshNetworkReport()
	// Full reconciliation remains available when watcher queues overflow.
	_ = c.reconcile(ctx, watcher)
	ticker := time.NewTicker(c.cfg.ReconcileInterval)
	defer ticker.Stop()
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	pending := false
	paths := make(map[string]struct{})
	fullScan := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("filesystem notification stream closed")
			}
			if len(paths) < 1024 {
				paths[event.Name] = struct{}{}
			} else {
				fullScan = true
			}
			if !pending {
				debounce.Reset(c.cfg.Debounce)
				pending = true
			}
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return errors.New("filesystem notification error stream closed")
			}
			c.setCollection("reconciling", watchErr)
			fullScan = true
			if !pending {
				debounce.Reset(c.cfg.Debounce)
				pending = true
			}
		case <-debounce.C:
			pending = false
			if fullScan {
				_ = c.reconcile(ctx, watcher)
			} else {
				for path := range paths {
					if err := c.refreshPath(ctx, watcher, path); err != nil {
						c.setCollection("blocked_source", err)
					}
				}
			}
			clear(paths)
			fullScan = false
			c.wake()
		case <-ticker.C:
			c.refreshNetworkReport()
			_ = c.reconcile(ctx, watcher)
		}
	}
}

func (c *Collector) refreshPath(ctx context.Context, watcher *sourceWatcher, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		return c.reconcile(ctx, watcher)
	}
	if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return nil
	}
	for _, root := range c.cfg.Roots {
		rel, err := filepath.Rel(root.Path, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return c.discoverFile(ctx, root.Provider, path, time.Now().UnixNano(), false)
	}
	return nil
}
