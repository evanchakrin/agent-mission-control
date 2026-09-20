package collector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// AdoptImportedSource seeds an as-yet undiscovered file from an explicit migration
// receipt. Call before starting collection. It verifies the imported prefix and
// retains the importer's exact 1 MiB chunk boundaries, including a short tail.
// No upload is acknowledged locally: the receiver must confirm every receipt.
// Payloads are reconstructed through the normal bounded upload recovery path.
// Existing catalog identities are never renamed or overwritten by adoption.
func (c *Collector) AdoptImportedSource(ctx context.Context, source protocol.Source, path, digest string) error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	valid := func(id string) bool { return id != "" && len(id) <= 512 && !strings.ContainsRune(id, 0) }
	hash, err := hex.DecodeString(digest)
	if err != nil || len(hash) != sha256.Size || digest != strings.ToLower(digest) ||
		source.MachineID != c.cfg.MachineID || !valid(source.SourceID) || !valid(source.Generation) ||
		source.GenerationSequence < 0 || source.Size < 0 {
		return errors.New("invalid imported source identity or digest")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	receipt, err := json.Marshal(struct {
		Source       protocol.Source
		Path, Digest string
	}{source, path, digest})
	if err != nil {
		return err
	}
	key := "import-adoption:" + source.SourceID
	var previous string
	err = c.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&previous)
	if err == nil {
		if previous == string(receipt) {
			return nil
		}
		return errors.New("source adoption receipt conflicts with this request")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	allowed := false
	for _, root := range c.cfg.Roots {
		if root.Provider != source.Provider {
			continue
		}
		resolved, e := filepath.EvalSymlinks(root.Path)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(resolved, path)
		if e == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("imported source is outside configured provider roots")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < source.Size {
		return errors.New("imported source prefix is unavailable")
	}
	identity, err := fileIdentity(f)
	if err != nil {
		return err
	}
	identity = source.Provider + ":" + identity
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sources WHERE id=? OR identity=?", source.SourceID, identity).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return errors.New("source already cataloged; adoption cannot replace existing history")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sources(id,identity,path,provider,native_id,current_generation,last_seen) VALUES(?,?,?,?,?,?,?)`, source.SourceID, identity, path, source.Provider, source.NativeID, source.Generation, time.Now().UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO generations(source_id,generation,generation_sequence,path,size,modified_ns,captured,verified_ns) VALUES(?,?,?,?,?,?,?,?)`, source.SourceID, source.Generation, source.GenerationSequence, path, info.Size(), info.ModTime().UnixNano(), source.Size, info.ModTime().UnixNano()); err != nil {
		return err
	}
	whole := sha256.New()
	buf := make([]byte, protocol.MaxChunkBytes)
	for offset := int64(0); offset < source.Size; {
		if err = ctx.Err(); err != nil {
			return err
		}
		length := min(int64(len(buf)), source.Size-offset)
		if _, err = io.ReadFull(f, buf[:length]); err != nil {
			return err
		}
		whole.Write(buf[:length])
		part := sha256.Sum256(buf[:length])
		partHash := hex.EncodeToString(part[:])
		key := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s", source.SourceID, source.Generation, offset, partHash)))
		name := hex.EncodeToString(key[:]) + ".chunk"
		if _, err = tx.ExecContext(ctx, `INSERT INTO chunks(source_id,generation,offset,length,sha256,filename,payload_present) VALUES(?,?,?,?,?,?,0)`, source.SourceID, source.Generation, offset, length, partHash, name); err != nil {
			return err
		}
		offset += length
	}
	if hex.EncodeToString(whole.Sum(nil)) != digest {
		return errors.New("imported source prefix checksum mismatch")
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return errors.New("source changed during adoption; retry after reconciliation")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?)", key, string(receipt)); err != nil {
		return err
	}
	return tx.Commit()
}
