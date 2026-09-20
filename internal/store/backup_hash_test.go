package store

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

// Cancel at a deterministic reader boundary, not after a machine-dependent
// timer. The real contextReader must consult Err between buffered disk reads.
type hashCancellationContext struct {
	context.Context
	checks int
}

func (c *hashCancellationContext) Err() error {
	c.checks++
	if c.checks >= 4 {
		return context.Canceled
	}
	return nil
}

func TestBackupHashCancellationAndDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger-fixture")
	data := bytes.Repeat([]byte("durable history\n"), 16384)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	got, err := fileHash(context.Background(), path)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatal("digest changed", got, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if digest, err := fileHash(cancelled, path+"-does-not-exist"); !errors.Is(err, context.Canceled) || digest != "" {
		t.Fatal("pre-cancelled hash touched filesystem or returned a digest", digest, err)
	}
	ctx := &hashCancellationContext{Context: context.Background()}
	if digest, err := fileHash(ctx, path); !errors.Is(err, context.Canceled) || digest != "" {
		t.Fatal("cancelled streaming hash returned a digest", digest, err, ctx.checks)
	}
}
