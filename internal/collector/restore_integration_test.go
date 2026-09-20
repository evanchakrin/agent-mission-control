package collector

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestRealHubBackupRestoreCollectorReconciliation(t *testing.T) {
	for _, scenario := range []struct {
		name                                   string
		missing, rewritten, retainSpool, moved bool
	}{
		{name: "source-retained"},
		{name: "source-and-spool-lost", missing: true},
		{name: "source-rewritten-after-spool-reclamation", rewritten: true},
		{name: "source-lost-spool-retained", missing: true, retainSpool: true},
		{name: "source-moved-after-spool-reclamation", moved: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			original, err := store.Open(t.TempDir(), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			const token = "isolated-restore-integration-token"
			var active atomic.Pointer[hub.Hub]
			active.Store(hub.New(original, token, "fixture"))
			c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) { active.Load().IngestionHandler().ServeHTTP(w, r) })
			c.cfg.Token = token
			before := []byte("before backup\n")
			all := []byte("before backup\nafter backup\n")
			path := writeSource(t, root, "history.jsonl", before)
			reconcile(t, c)
			capture(t, c, 1)
			if _, err = c.UploadOnce(ctx); err != nil {
				t.Fatal(err)
			}
			var source, generation string
			if err = c.db.QueryRow("SELECT source_id,generation FROM chunks LIMIT 1").Scan(&source, &generation); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(t.TempDir(), "backup")
			if _, err = original.Backup(ctx, backup); err != nil {
				t.Fatal(err)
			}
			writeSource(t, root, "history.jsonl", all)
			reconcile(t, c)
			capture(t, c, 1)
			if _, err = c.UploadOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if st := stats(t, c); st.BacklogBytes != 0 {
				t.Fatal("fixture append not acknowledged", st)
			}
			if _, err = c.db.Exec("UPDATE chunks SET ack_at=?", time.Now().Add(-25*time.Hour).UnixNano()); err != nil {
				t.Fatal(err)
			}
			if !scenario.retainSpool {
				if err = c.reclaim(ctx, false, 0); err != nil {
					t.Fatal(err)
				}
				if st := stats(t, c); st.SpoolBytes != 0 {
					t.Fatal("fixture spool not reclaimed", st)
				}
			} else if stats(t, c).SpoolBytes == 0 {
				t.Fatal("fixture failed to retain spool bytes")
			}
			if scenario.missing {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.rewritten {
				// Same length is deliberate: size alone cannot certify the old
				// chunk. Recovery must compare its original content checksum.
				writeSource(t, root, "history.jsonl", bytes.Repeat([]byte("x"), len(all)))
			}
			if scenario.moved {
				movedPath := filepath.Join(root, "moved-history.jsonl")
				if err = os.Rename(path, movedPath); err != nil {
					t.Fatal(err)
				}
				reconcile(t, c)
				var currentSource, currentGeneration, currentPath string
				if err = c.db.QueryRow(`SELECT id,current_generation,path FROM sources`).Scan(&currentSource, &currentGeneration, &currentPath); err != nil {
					t.Fatal(err)
				}
				if currentSource != source || currentGeneration != generation || currentPath != movedPath {
					t.Fatal("move changed capture identity", currentSource, currentGeneration, currentPath)
				}
			}
			restored, err := store.RestoreBackup(ctx, backup, filepath.Join(t.TempDir(), "restored"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if restored.RecoveryEpoch() == original.RecoveryEpoch() {
				t.Fatal("restore did not rotate epoch")
			}
			active.Store(hub.New(restored, token, "fixture"))
			if err = c.sendHeartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			var pending string
			if err = c.db.QueryRow("SELECT value FROM settings WHERE key='recovery_pending'").Scan(&pending); err != nil || pending != restored.RecoveryEpoch() {
				t.Fatal("heartbeat did not schedule recovery", err)
			}
			if err = c.ReconcileRemote(ctx); err != nil {
				t.Fatal(err)
			}
			if st := stats(t, c); st.BacklogBytes != int64(len(all)-len(before)) {
				t.Fatal("wrong post-backup backlog", st)
			}
			_, uploadErr := c.UploadOnce(ctx)
			want := all
			if (scenario.missing || scenario.rewritten) && !scenario.retainSpool {
				want = before
				if uploadErr == nil || stats(t, c).Gaps == 0 || stats(t, c).BacklogBytes == 0 {
					t.Fatal("unrecoverable history not reported", uploadErr)
				}
				gaps := stats(t, c).Gaps
				if err = c.ReconcileRemote(ctx); err != nil {
					t.Fatal(err)
				}
				// Advance only this fixture past its persisted retry pause so the
				// next assertion exercises another upload attempt, not idle backoff.
				if _, err = c.db.Exec("DELETE FROM settings WHERE key='upload_pause'"); err != nil {
					t.Fatal(err)
				}
				if _, err = c.UploadOnce(ctx); err == nil || stats(t, c).Gaps != gaps {
					t.Fatal("retry duplicated or hid an unrecoverable gap", err)
				}
			} else if uploadErr != nil || stats(t, c).BacklogBytes != 0 || stats(t, c).Gaps != 0 {
				t.Fatal("retained source did not reconcile", uploadErr)
			}
			if err = c.sendHeartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			machines, err := restored.ListMachines(ctx)
			if err != nil || len(machines) != 1 || machines[0].Heartbeat.HistoryGaps == nil || *machines[0].Heartbeat.HistoryGaps != stats(t, c).Gaps {
				t.Fatal("history gaps did not reach the restored hub", err)
			}
			reader, err := restored.OpenSource(ctx, source, generation, 0)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(actual, want) {
				t.Fatal("restored bytes differ", err)
			}
			reader, err = original.OpenSource(ctx, source, generation, 0)
			if err != nil {
				t.Fatal(err)
			}
			actual, err = io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(actual, all) {
				t.Fatal("original ledger changed during restore", err)
			}
		})
	}
}
