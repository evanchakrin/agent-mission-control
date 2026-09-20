package indexer

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestNativeRebuildPreparationAndHubPublication(t *testing.T) {
	binary := os.Getenv("AMC_RECOVERY_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	for _, partial := range []bool{false, true} {
		dir := t.TempDir()
		x, work, sources := groupSourcesAt(t, dir, groupRecord, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
		func() {
			defer cancel()
			defer worker.Close()
			if _, err := x.Once(ctx, work[0].SourceID, work[0].Generation); err != nil {
				t.Fatal(err)
			}
			src := sources[0]
			if partial {
				appendRebuildRaw(t, x.Store, &src, []byte(`{"type":`))
			}
			sessionID := parser.SessionID(src)
			yes := true
			if _, err := x.Store.PatchMetadata(ctx, sessionID, store.MetadataPatch{OperationID: "keep-archive", Archived: &yes}); err != nil {
				t.Fatal(err)
			}
			job, err := x.Store.BeginRebuild(ctx, sessionID, "native-rebuild", parser.Version)
			if err != nil {
				t.Fatal(err)
			}
			before, err := x.Store.ProjectionRevision(ctx, job.Revision)
			if err != nil {
				t.Fatal(err)
			}
			active, err := x.Store.SourceState(ctx, src.SourceID, src.Generation)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := worker.PrepareRebuild(ctx, job.Revision)
			if err != nil || prepared.Batch == nil || prepared.Batch.ProjectionRevision != job.Revision {
				t.Fatal(prepared, err)
			}
			after, err := x.Store.ProjectionRevision(ctx, job.Revision)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("child changed staged revision", err)
			}
			current, err := x.Store.SourceState(ctx, src.SourceID, src.Generation)
			if err != nil || !reflect.DeepEqual(active, current) {
				t.Fatal("child changed active checkpoint", err)
			}
			// Drop an uncommitted preparation and restart the child. Rebuilding
			// must reread evidence rather than rely on child memory.
			if err := worker.Close(); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 10; i++ {
				if _, err = x.RebuildPrepared(ctx, job.Revision, worker.PrepareRebuild); err != nil {
					t.Fatal(err)
				}
				after, err = x.Store.ProjectionRevision(ctx, job.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if after.State == "active" {
					break
				}
			}
			if after.State != "active" {
				t.Fatal("rebuild did not publish", after.State)
			}
			row, err := x.Store.GetSession(ctx, sessionID)
			if err != nil || row.TokensIn != 10 || row.TokensOut != 2 || !row.Metadata.Archived || row.ProjectionRevision != job.Revision {
				t.Fatal("rebuild changed history/organization", row, err)
			}
			if partial && row.Completeness != "indexed-source-partial" {
				t.Fatal("partial tail hidden", row.Completeness)
			}
			if n, err := x.RebuildPrepared(ctx, job.Revision, worker.PrepareRebuild); err != nil || n != 0 {
				t.Fatal("published revision repeated", n, err)
			}
		}()
	}
}
