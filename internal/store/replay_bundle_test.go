package store

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestReplayBundleCannotCompleteAcrossSourceOrOrganizationChanges(t *testing.T) {
	for _, kind := range []string{"append", "organization", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := publishedRevisionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &changingExportWriter{change: func() {
				switch kind {
				case "append":
					ingest(t, s, testSource(), 13, "new bytes\n")
				case "organization":
					yes := true
					if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "during-bundle", Archived: &yes}); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				}
			}}
			err := s.WriteReplayBundle(ctx, "session-1", bytes.Repeat([]byte("x"), 8192), w)
			want := ErrHistoryChanged
			if kind == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("changed bundle completed", err)
			}
			if _, err = zip.NewReader(bytes.NewReader(w.Bytes()), int64(w.Len())); err == nil {
				t.Fatal("interrupted bundle has valid central directory")
			}
		})
	}
}

func TestReplayBundlePropagatesWriterFailure(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	if err := s.WriteReplayBundle(context.Background(), "session-1", []byte("viewer"), failedExportWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}
