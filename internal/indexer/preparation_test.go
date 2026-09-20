package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
)

func TestPreparationDoesNotNeedSQLiteWriter(t *testing.T) {
	for _, raw := range [][]byte{groupRecord, bytes.TrimSuffix(groupRecord, []byte("\n"))} {
		dir := t.TempDir()
		x, work, _ := groupSourcesAt(t, dir, raw, 1)
		db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer db.Close()
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			prepared, err := x.PrepareOnce(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatalf("preparation required writer access: %v", err)
			}
			if prepared.Batch == nil && prepared.Scan == nil {
				t.Fatal("preparation did not produce work")
			}
		}()
	}
}

func TestPreparationDoesNotPublishCompleteOrPartialRecords(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "complete"
		raw := groupRecord
		if partial {
			name = "partial"
			raw = bytes.TrimSuffix(groupRecord, []byte("\n"))
		}
		t.Run(name, func(t *testing.T) {
			x, work, _ := groupSources(t, raw, 1)
			ctx := context.Background()
			before, err := x.Store.SourceState(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := x.PrepareOnce(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			after, err := x.Store.SourceState(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("preparation changed durable source state")
			}
			// Repeating preparation before publication must produce the same work.
			again, err := x.PrepareOnce(ctx, work[0].SourceID, work[0].Generation)
			if err != nil || !reflect.DeepEqual(prepared, again) {
				t.Fatalf("unstable preparation: %v", err)
			}
			if partial {
				if prepared.Records != 0 || prepared.Batch != nil || prepared.Scan == nil {
					t.Fatalf("partial record published as batch: %+v", prepared)
				}
				if err = x.Store.SaveParserScan(ctx, prepared.Scan.Source, prepared.Scan.Checkpoint); err != nil {
					t.Fatal(err)
				}
			} else {
				if prepared.Records != 1 || prepared.Batch == nil || prepared.Scan != nil {
					t.Fatalf("complete record missing batch: %+v", prepared)
				}
				if err = x.Store.CommitIndex(ctx, *prepared.Batch); err != nil {
					t.Fatal(err)
				}
			}
			published, err := x.Store.SourceState(ctx, work[0].SourceID, work[0].Generation)
			if err != nil {
				t.Fatal(err)
			}
			if partial {
				state, err := parser.DecodeState(published.ParserState)
				if err != nil || state.ScanOffset != int64(len(raw)) || published.IndexedOffset != 0 {
					t.Fatalf("invalid scan publication: %+v, %v", published, err)
				}
			} else if published.IndexedOffset != int64(len(raw)) {
				t.Fatal("batch was not durably published")
			}
			empty, err := x.PrepareOnce(ctx, work[0].SourceID, work[0].Generation)
			if err != nil || empty.Records != 0 || empty.Batch != nil || empty.Scan != nil {
				t.Fatalf("published work repeated: %+v, %v", empty, err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			failed, err := x.PrepareOnce(canceled, work[0].SourceID, work[0].Generation)
			if err == nil || !reflect.DeepEqual(failed, Preparation{}) {
				t.Fatal("canceled preparation returned work")
			}
		})
	}
}
