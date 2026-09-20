package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIndexPublicationAbandonsCanceledWriterAdmission(t *testing.T) {
	for _, kind := range []string{"batch", "group", "scan"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestStore(t, Options{})
			batches := indexGroupFixture(t, s, 1)
			b := batches[0]
			before, err := s.SourceState(context.Background(), b.SourceID, b.Generation)
			if err != nil {
				t.Fatal(err)
			}
			publish := func(ctx context.Context) error {
				switch kind {
				case "batch":
					return s.CommitIndex(ctx, b)
				case "group":
					return s.CommitIndexGroup(ctx, batches)
				default:
					return s.SaveParserScan(ctx, before, []byte(`{"scanOffset":2}`))
				}
			}
			s.writeMu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- publish(ctx) }()
			select {
			case err = <-done:
				s.writeMu.Unlock()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				s.writeMu.Unlock()
				<-done
				t.Fatal("canceled publication waited for writer")
			}
			after, err := s.SourceState(context.Background(), b.SourceID, b.Generation)
			if err != nil || after.IndexedOffset != before.IndexedOffset || string(after.ParserState) != string(before.ParserState) {
				t.Fatal("cancellation changed checkpoint", after, err)
			}
			if err = publish(context.Background()); err != nil {
				t.Fatal("writer unusable after canceled request", err)
			}
		})
	}
}

func TestRebuildAbandonsCanceledWriterAdmission(t *testing.T) {
	for _, kind := range []string{"verify", "checkpoint", "publish"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestStore(t, Options{})
			s.writeMu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "verify":
					_, err = s.VerifyRebuildBatch(ctx, "missing")
				case "checkpoint":
					_, err = s.saveVerification(ctx, ProjectionRevision{}, nil, verificationCheckpoint{}, false)
				case "publish":
					err = s.PublishRebuild(ctx, "missing")
				}
				done <- err
			}()
			select {
			case err := <-done:
				s.writeMu.Unlock()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				s.writeMu.Unlock()
				<-done
				t.Fatal("canceled rebuild waited for writer")
			}
		})
	}
}
