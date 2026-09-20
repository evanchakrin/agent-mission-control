// Package backup creates explicitly requested complete installation snapshots.
// It never schedules backups, stops services, installs configuration, or removes
// an existing destination. Partial output remains inspectable after failure.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/desktop"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type Manifest struct {
	Version      int                         `json:"version"`
	CreatedAt    time.Time                   `json:"createdAt"`
	Hub          store.BackupManifest        `json:"hub"`
	Desktop      desktop.StateBackupManifest `json:"desktop"`
	ConfigSHA256 map[string]string           `json:"configSHA256"`
	Warning      string                      `json:"warning"`
}

func within(root, target string) bool {
	if strings.EqualFold(root, target) {
		return true
	}
	r, err := filepath.Rel(strings.ToLower(root), strings.ToLower(target))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func destination(path string, sources ...string) (string, error) {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return "", errors.New("backup destination must be an explicit absolute local path")
	}
	path = filepath.Clean(path)
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if filepath.Dir(path) == path {
		return "", errors.New("backup destination cannot be a disk root")
	}
	for _, source := range sources {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return "", err
		}
		if within(resolved, path) || within(path, resolved) {
			return "", errors.New("backup and active state must not overlap")
		}
	}
	if err = os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	return path, nil
}
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func publish(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func Create(ctx context.Context, hub *store.Store, hubConfig, desktopConfig platform.Config, dest string) (Manifest, error) {
	var manifest Manifest
	if hubConfig.Role != platform.Hub || desktopConfig.Role != platform.Desktop {
		return manifest, errors.New("complete backup requires hub and desktop configurations")
	}
	dir, err := destination(dest, hubConfig.DataDir, desktopConfig.DataDir)
	if err != nil {
		return manifest, err
	}
	manifest.Version = 1
	manifest.CreatedAt = time.Now().UTC()
	manifest.ConfigSHA256 = map[string]string{}
	manifest.Warning = "Manual snapshot only. Post-backup accepted data can be unrecoverable after hub disk loss if collectors no longer retain a copy. Component snapshots are individually transactional, not one cross-process transaction."
	manifest.Hub, err = hub.Backup(ctx, filepath.Join(dir, "hub"))
	if err != nil {
		return manifest, err
	}
	manifest.Desktop, err = desktop.SnapshotState(ctx, desktopConfig.DataDir, filepath.Join(dir, "desktop"))
	if err != nil {
		return manifest, err
	}
	for filename, config := range map[string]platform.Config{"hub-config.json": hubConfig, "desktop-config.json": desktopConfig} {
		path := filepath.Join(dir, filename)
		if err = platform.WriteProtectedConfig(path, config); err != nil {
			return manifest, err
		}
		hash, e := fileHash(path)
		if e != nil {
			return manifest, e
		}
		manifest.ConfigSHA256[filename] = hash
	}
	if err = publish(filepath.Join(dir, "installation-manifest.json"), manifest); err != nil {
		return manifest, err
	}
	return Verify(ctx, dir)
}
func Verify(ctx context.Context, dir string) (Manifest, error) {
	var m Manifest
	if err := ctx.Err(); err != nil {
		return m, err
	}
	f, err := os.Open(filepath.Join(dir, "installation-manifest.json"))
	if err != nil {
		return m, err
	}
	const manifestLimit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(f, manifestLimit+1))
	closeErr := f.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	if err = ctx.Err(); err != nil {
		return m, err
	}
	if len(raw) > manifestLimit {
		return m, errors.New("installation backup manifest exceeds size limit")
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Version != 1 {
		return m, errors.New("unsupported installation backup")
	}
	h, err := store.VerifyBackup(ctx, filepath.Join(dir, "hub"))
	if err != nil {
		return m, err
	}
	if h.DatabaseSHA256 != m.Hub.DatabaseSHA256 {
		return m, errors.New("hub manifest mismatch")
	}
	d, err := desktop.VerifyStateBackup(ctx, filepath.Join(dir, "desktop"))
	if err != nil {
		return m, err
	}
	if d.DatabaseSHA256 != m.Desktop.DatabaseSHA256 {
		return m, errors.New("desktop manifest mismatch")
	}
	for _, name := range []string{"hub-config.json", "desktop-config.json"} {
		hash, err := fileHash(filepath.Join(dir, name))
		if err != nil {
			return m, err
		}
		if hash != m.ConfigSHA256[name] {
			return m, errors.New("configuration checksum mismatch")
		}
	}
	return m, nil
}
func Restore(ctx context.Context, source, dest string) (Manifest, error) {
	manifest, err := Verify(ctx, source)
	if err != nil {
		return manifest, err
	}
	dir, err := destination(dest, source)
	if err != nil {
		return manifest, err
	}
	s, err := store.RestoreBackup(ctx, filepath.Join(source, "hub"), filepath.Join(dir, "hub"), store.Options{})
	if err != nil {
		return manifest, err
	}
	if err = s.Close(); err != nil {
		return manifest, err
	}
	if _, err = desktop.RestoreStateBackup(ctx, filepath.Join(source, "desktop"), filepath.Join(dir, "desktop")); err != nil {
		return manifest, err
	}
	for _, name := range []string{"hub-config.json", "desktop-config.json"} {
		c, err := platform.LoadConfig(filepath.Join(source, name))
		if err != nil {
			return manifest, err
		}
		c.DataDir = filepath.Join(dir, string(c.Role))
		if err = platform.WriteProtectedConfig(filepath.Join(dir, name), c); err != nil {
			return manifest, err
		}
	}
	return manifest, publish(filepath.Join(dir, "restored-from.json"), map[string]any{"backup": source, "restoredAt": time.Now().UTC(), "installed": false, "reconcileCollectors": true})
}
