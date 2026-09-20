package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type backupHashCancellation struct {
	context.Context
	checks int
}

func (c *backupHashCancellation) Err() error {
	c.checks++
	if c.checks >= 4 {
		return context.Canceled
	}
	return nil
}

func TestOwnerBackupHashCancellationAndDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-fixture")
	data := bytes.Repeat([]byte("saved guidance\n"), 16384)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if got, err := stateFileHash(context.Background(), path); err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatal("owner hash changed", got, err)
	}
	ctx := &backupHashCancellation{Context: context.Background()}
	if got, err := stateFileHash(ctx, path); !errors.Is(err, context.Canceled) || got != "" {
		t.Fatal("mid-stream cancellation returned a digest", got, err, ctx.checks)
	}
}
