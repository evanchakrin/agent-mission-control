package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func readVerification(t *testing.T, s *Store, id string) verificationCheckpoint {
	t.Helper()
	var data []byte
	if err := s.db.QueryRow(`SELECT verification FROM projection_revisions WHERE revision=?`, id).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var cp verificationCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestVerificationResumesBoundedPagesAcrossBackupAndRestart(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	r, err := s.BeginRebuild(ctx, "session-1", "many-rows", "3")
	if err != nil {
		t.Fatal(err)
	}
	b := batch(testSource(), 0, 13)
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	for i := 0; i < 500; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("e-%d", i), SourceLength: 13})
		b.Usage = append(b.Usage, UsageObservation{ID: fmt.Sprintf("u-%d", i), TokensIn: 1})
	}
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if done, err := s.VerifyRebuildBatch(ctx, r.Revision); err != nil || done {
		t.Fatal(done, err)
	}
	cp := readVerification(t, s, r.Revision)
	if cp.Events != 256 || cp.Phase != "events" || cp.Tokens[0] != 0 {
		t.Fatal("first dispatch was unbounded", cp)
	}
	if err = s.PublishRebuild(ctx, r.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("unverified history published", err)
	}
	dest := filepath.Join(t.TempDir(), "backup")
	if _, err = s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(ctx, dest, filepath.Join(t.TempDir(), "restore"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if done, err := restored.VerifyRebuildBatch(ctx, r.Revision); err != nil || done {
		t.Fatal(done, err)
	}
	cp = readVerification(t, restored, r.Revision)
	if cp.Events != 500 || cp.Tokens[0] != 256 || cp.Phase != "usage" {
		t.Fatal("restore restarted or skipped a verification page", cp)
	}
	dir := restored.dir
	restored.Close()
	restored, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if done, err := restored.VerifyRebuildBatch(ctx, r.Revision); err != nil || !done {
		t.Fatal(done, err)
	}
	row, err := restored.GetSession(ctx, "session-1")
	if err != nil || row.TokensIn != 13 {
		t.Fatal("verification changed published totals", row, err)
	}
	if err = restored.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	row, err = restored.GetSession(ctx, "session-1")
	if err != nil || row.TokensIn != 500 || row.EventCount != 500 {
		t.Fatal(row, err)
	}
}

func TestVerificationBoundsPartialTailAndRejectsLaterCompleteRecord(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			s := openTestStore(t, Options{})
			src := testSource()
			ctx := context.Background()
			chunk := strings.Repeat("x", 1<<20)
			for i := 0; i < 9; i++ {
				data := chunk
				if complete && i == 8 {
					data = data[:len(data)-1] + "\n"
				}
				ingest(t, s, src, int64(i<<20), data)
			}
			if err := s.CommitIndex(ctx, batch(src, 0, 0)); err != nil {
				t.Fatal(err)
			}
			r, err := s.BeginRebuild(ctx, "session-1", "tail", "2")
			if err != nil {
				t.Fatal(err)
			}
			for step := 1; step <= 2; step++ {
				if done, err := s.VerifyRebuildBatch(ctx, r.Revision); err != nil || done {
					t.Fatal(done, err)
				}
				cp := readVerification(t, s, r.Revision)
				if cp.TailOffset != int64(step*(4<<20)) {
					t.Fatal("tail dispatch exceeded or lost its budget", cp)
				}
			}
			done, err := s.VerifyRebuildBatch(ctx, r.Revision)
			if complete {
				if done || !errors.Is(err, ErrConflict) {
					t.Fatal("skipped complete record", done, err)
				}
			} else {
				if err != nil || !done {
					t.Fatal(done, err)
				}
				r, err = s.ProjectionRevision(ctx, r.Revision)
				if err != nil || r.Session.Completeness != "indexed-source-partial" {
					t.Fatal(r, err)
				}
			}
		})
	}
}

func TestDamagedVerificationCheckpointFailsClosed(t *testing.T) {
	for _, value := range []string{"null", "{", "{\"phase\":\"unknown\"}", "{\"events\":-1}"} {
		t.Run(value, func(t *testing.T) {
			s, _ := publishedRevisionFixture(t)
			r, err := s.BeginRebuild(context.Background(), "session-1", "damaged", "2")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`UPDATE projection_revisions SET verification=? WHERE revision=?`, value, r.Revision); err != nil {
				t.Fatal(err)
			}
			if done, err := s.VerifyRebuildBatch(context.Background(), r.Revision); done || !errors.Is(err, ErrInvalid) {
				t.Fatal(done, err)
			}
		})
	}
}

func TestVerificationMigrationAndCheckpointCAS(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	r, err := s.BeginRebuild(ctx, "session-1", "upgrade", "2")
	if err != nil {
		t.Fatal(err)
	}
	dropLedgerTotalsForLegacyFixture(t, s)
	if _, err = s.db.Exec(`ALTER TABLE projection_revisions DROP COLUMN verification; PRAGMA user_version=4;`); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`UPDATE projection_revisions SET state='verifying' WHERE revision=?`, r.Revision); err != nil {
		t.Fatal(err)
	}
	r, err = s.ProjectionRevision(ctx, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	cp := verificationCheckpoint{Phase: "events"}
	if _, err = s.saveVerification(ctx, r, []byte(`{}`), cp, false); err != nil {
		t.Fatal("migration text default did not compare to bytes", err)
	}
	if _, err = s.saveVerification(ctx, r, []byte(`{}`), cp, false); !errors.Is(err, ErrConflict) {
		t.Fatal("stale verifier overwrote committed progress", err)
	}
	row, err := s.GetSession(ctx, "session-1")
	if err != nil || row.TokensIn != 13 {
		t.Fatal("migration changed published history", row, err)
	}
}
