package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestNativeWorkerPreparationLeavesPublicationToHub(t *testing.T) {
	binary := os.Getenv("AMC_RECOVERY_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary path required")
	}
	for _, raw := range [][]byte{groupRecord, groupRecord[:len(groupRecord)-1]} {
		dir := t.TempDir()
		x, work, _ := groupSourcesAt(t, dir, raw, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
		func() {
			defer cancel()
			defer worker.Close()
			before, err := x.Store.SourceState(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := worker.Prepare(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			after, err := x.Store.SourceState(ctx, work[0].SourceID, work[0].Generation)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("child published preparation", err)
			}
			if prepared.Batch != nil {
				if prepared.Records != 1 || prepared.Scan != nil {
					t.Fatal(prepared)
				}
				err = x.Store.CommitIndex(ctx, *prepared.Batch)
			} else if prepared.Scan != nil {
				if prepared.Records != 0 {
					t.Fatal(prepared)
				}
				err = x.Store.SaveParserScan(ctx, prepared.Scan.Source, prepared.Scan.Checkpoint)
			} else {
				t.Fatal("missing preparation")
			}
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := worker.Prepare(ctx, work[0].SourceID, work[0].Generation)
			if err != nil || repeated.Records != 0 || repeated.Batch != nil || repeated.Scan != nil {
				t.Fatal("duplicate preparation", repeated, err)
			}
		}()
	}
}

func TestNativeWorkerHubGroupRecoveryUsesFreshPreparations(t *testing.T) {
	binary := os.Getenv("AMC_RECOVERY_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	for _, uncertain := range []bool{false, true} {
		dir := t.TempDir()
		x, work, sources := groupSourcesAt(t, dir, groupRecord, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
		func() {
			defer cancel()
			defer worker.Close()
			calls := 0
			prepare := func(ctx context.Context, id, generation string) (Preparation, error) {
				calls++
				return worker.Prepare(ctx, id, generation)
			}
			results, err := x.publishGroup(ctx, work, prepare, func(ctx context.Context, batches []store.IndexBatch) error {
				for _, src := range sources {
					state, e := x.Store.SourceState(ctx, src.SourceID, src.Generation)
					if e != nil || state.IndexedOffset != 0 {
						t.Fatal("child published before hub", state, e)
					}
				}
				if uncertain {
					if e := x.Store.CommitIndexGroup(ctx, batches); e != nil {
						t.Fatal(e)
					}
				}
				return errors.New("simulated failed or lost hub commit")
			})
			if err != nil || calls != 16 {
				t.Fatal("recovery did not reprepare in child", calls, err)
			}
			for i, src := range sources {
				want := int64(1)
				if uncertain {
					want = 0
				}
				if results[i].Error != "" || results[i].Records != want {
					t.Fatal(results)
				}
				session, e := x.Store.GetSession(ctx, parser.SessionID(src))
				if e != nil || session.TokensIn != 10 || session.TokensOut != 2 {
					t.Fatal(session, e)
				}
			}
		}()
	}
}

func TestNativeGroupWorkerRecoversAfterCancellation(t *testing.T) {
	binary := os.Getenv("AMC_RECOVERY_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary path required")
	}
	dir := t.TempDir()
	x, work, sources := groupSourcesAt(t, dir, groupRecord, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
	defer worker.Close()
	// Warm the persistent child without indexing a fixture source.
	if _, err := worker.Process(ctx, "missing", "g"); err == nil {
		t.Fatal("missing source accepted")
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	blocked, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	_, err = worker.ProcessGroup(blocked, work)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expected cancellation", err)
	}
	if worker.child != nil {
		t.Fatal("canceled parser child retained")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, src := range sources {
		state, e := x.Store.SourceState(ctx, src.SourceID, src.Generation)
		if e != nil || state.IndexedOffset != 0 || state.DurableOffset != src.Size {
			t.Fatal("killed group changed evidence", state, e)
		}
	}
	results, err := worker.ProcessGroup(ctx, work)
	if err != nil || len(results) != len(work) {
		t.Fatal(results, err)
	}
	for i, src := range sources {
		if results[i].Error != "" || results[i].Records != 1 {
			t.Fatal(results)
		}
		session, e := x.Store.GetSession(ctx, parser.SessionID(src))
		if e != nil || session.TokensIn != 10 || session.TokensOut != 2 {
			t.Fatal("recovered group accounting", session, e)
		}
	}
	results, err = worker.ProcessGroup(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Records != 0 || r.Error != "" {
			t.Fatal("replay duplicated progress", results)
		}
	}
}
