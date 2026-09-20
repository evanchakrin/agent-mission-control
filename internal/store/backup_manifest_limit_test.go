package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type countedManifestReader struct {
	io.Reader
	read int
}

func (r *countedManifestReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestBackupManifestReadBoundAndCancellation(t *testing.T) {
	r := &countedManifestReader{Reader: io.LimitReader(zeroManifestReader{}, 1<<30)}
	if _, err := readBackupManifest(context.Background(), r); err == nil || r.read != (1<<20)+1 {
		t.Fatal("oversize manifest read beyond bounded prefix", r.read, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = &countedManifestReader{Reader: strings.NewReader(`{"version":1}`)}
	if _, err := readBackupManifest(ctx, r); !errors.Is(err, context.Canceled) || r.read != 0 {
		t.Fatal("cancelled manifest read consumed input", r.read, err)
	}
	for _, raw := range []string{`{"version":1}`, `{"version":2,"legacyAssetsSHA256":"` + strings.Repeat("a", 64) + `"}`} {
		if _, err := readBackupManifest(context.Background(), strings.NewReader(raw)); err != nil {
			t.Fatal("supported manifest rejected", err)
		}
	}
	if _, err := readBackupManifest(context.Background(), strings.NewReader(`{"version":1} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

type zeroManifestReader struct{}

func (zeroManifestReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
