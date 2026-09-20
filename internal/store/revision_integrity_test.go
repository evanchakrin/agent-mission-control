package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publishedRevisionFixture(t *testing.T) (*Store, ProjectionRevision) {
	t.Helper()
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "event", Text: "baseline", SourceLength: 13}}
	b.Usage = []UsageObservation{{ID: "usage", TokensIn: 13}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "rebuild", "2")
	if err != nil {
		t.Fatal(err)
	}
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"2"}`)
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestBackupRejectsBrokenProjectionReferences(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{"pointer", `UPDATE active_projection SET revision='missing'`, "active revision pointer"},
		{"bounds", `UPDATE projection_revisions SET target_offset=999999`, "revision source or bounds"},
		{"checkpoint", `UPDATE sources SET parser_state='{}'`, "active checkpoint"},
		{"session", `UPDATE sessions SET projection=json_set(projection,'$.projectionRevision','')`, "published session revision"},
		{"event", `UPDATE events SET projection_revision='missing' WHERE projection_revision<>''`, "event revision reference"},
		{"usage", `UPDATE usage_observations SET projection_revision='missing' WHERE projection_revision<>''`, "usage revision reference"},
		{"baseline", `UPDATE baseline_projections SET indexed_offset=-1`, "baseline source or bounds"},
		{"pricing", `UPDATE sessions SET projection=json_set(projection,'$.pricing',json('{"snapshotId":"missing"}'))`, "pricing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := publishedRevisionFixture(t)
			ctx := context.Background()
			if err := checkProjectionIntegrity(ctx, s.db); err != nil {
				t.Fatal("valid fixture rejected", err)
			}
			if _, err := s.db.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "backup")
			if _, err := s.Backup(ctx, dest); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("bad projection accepted", err)
			}
			if _, err := os.Stat(filepath.Join(dest, "manifest.json")); !os.IsNotExist(err) {
				t.Fatal("failed backup published manifest", err)
			}
		})
	}
}

func TestVerifyBackupRejectsLogicalRevisionDamageWithValidDigest(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "backup")
	m, err := s.Backup(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBackup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dest, "ledger.sqlite")
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE active_projection SET revision='missing'`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	m.DatabaseSHA256, err = fileHash(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	if err = os.WriteFile(filepath.Join(dest, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBackup(ctx, dest); err == nil || !strings.Contains(err.Error(), "active revision pointer") {
		t.Fatal("digest hid logical corruption", err)
	}
}
