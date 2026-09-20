package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// This opt-in comparison changes only dedicated connections in fresh ledgers.
// It does not select a production checkpoint policy or certify API latency.
func TestCheckpointModesWithRotatingReadersDiagnostic(t *testing.T) {
	if testing.Short() || os.Getenv("AMC_CHECKPOINT_MODES_DIAGNOSTIC") != "1" {
		t.Skip("opt-in concurrent checkpoint comparison")
	}
	for _, policy := range []struct {
		name, mode string
		wait       int
	}{
		{"passive", "PASSIVE", 0},
		{"restart-no-wait", "RESTART", 0},
		{"restart-5ms", "RESTART", 5},
		{"restart-25ms", "RESTART", 25},
	} {
		t.Run(policy.name, func(t *testing.T) {
			s := openTestStore(t, Options{ExternalCheckpointOwner: true})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			writer, err := s.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			checkpoint, err := s.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer checkpoint.Close()
			if _, err = writer.ExecContext(ctx, `PRAGMA journal_size_limit=1048576; CREATE TABLE rotating_fixture(id INTEGER PRIMARY KEY, data BLOB); INSERT INTO rotating_fixture(data) VALUES(x'01')`); err != nil {
				t.Fatal(err)
			}
			// This fixture-only bound limits lock waiting, not filesystem I/O.
			if _, err = checkpoint.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", policy.wait)); err != nil {
				t.Fatal(err)
			}
			work, stop := context.WithCancel(ctx)
			var wg sync.WaitGroup
			var mu sync.Mutex
			var reads, checkpoints []time.Duration
			var failures []error
			var busyCount int
			recordFailure := func(e error) {
				if e != nil && work.Err() == nil {
					mu.Lock()
					failures = append(failures, e)
					mu.Unlock()
				}
			}
			defer func() { stop(); wg.Wait() }()
			for reader := 0; reader < 2; reader++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for work.Err() == nil {
						started := time.Now()
						tx, e := s.db.BeginTx(work, &sql.TxOptions{ReadOnly: true})
						if e != nil {
							recordFailure(e)
							return
						}
						var count int
						e = tx.QueryRowContext(work, `SELECT count(*) FROM rotating_fixture`).Scan(&count)
						mu.Lock()
						if len(reads) < 10000 {
							reads = append(reads, time.Since(started))
						}
						mu.Unlock()
						if e != nil {
							tx.Rollback()
							recordFailure(e)
							return
						}
						select {
						case <-work.Done():
						case <-time.After(8 * time.Millisecond):
						}
						tx.Rollback()
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-work.Done():
						return
					case <-ticker.C:
					}
					started := time.Now()
					var busy, frames, copied int
					e := checkpoint.QueryRowContext(work, "PRAGMA wal_checkpoint("+policy.mode+")").Scan(&busy, &frames, &copied)
					mu.Lock()
					if len(checkpoints) < 10000 {
						checkpoints = append(checkpoints, time.Since(started))
					}
					if busy != 0 {
						busyCount++
					}
					mu.Unlock()
					if e != nil {
						recordFailure(e)
						return
					}
				}
			}()
			var writes []time.Duration
			var peakWAL int64
			started := time.Now()
			for i := 0; i < 256; i++ {
				before := time.Now()
				if _, err = writer.ExecContext(ctx, `INSERT INTO rotating_fixture(data) VALUES(zeroblob(65536))`); err != nil {
					t.Fatal(err)
				}
				writes = append(writes, time.Since(before))
				info, e := os.Stat(filepath.Join(s.dir, "ledger.sqlite-wal"))
				if e != nil {
					t.Fatal(e)
				}
				peakWAL = max(peakWAL, info.Size())
			}
			elapsed := time.Since(started)
			stop()
			wg.Wait()
			if len(failures) != 0 {
				t.Fatal(failures)
			}
			var count, full int
			var bytes int64
			if err = writer.QueryRowContext(ctx, `SELECT count(*),sum(length(data)) FROM rotating_fixture`).Scan(&count, &bytes); err != nil || count != 257 || bytes != 1+(256<<16) {
				t.Fatal(count, bytes, err)
			}
			if err = writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
				t.Fatal("durability changed", full, err)
			}
			var integrity string
			if err = writer.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatal(integrity, err)
			}
			stats := func(v []time.Duration) string {
				if len(v) == 0 {
					return "none"
				}
				sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
				return fmt.Sprintf("n=%d p95=%s max=%s", len(v), v[(len(v)*95+99)/100-1], v[len(v)-1])
			}
			t.Logf("mode=%s wait=%dms elapsed=%s peakWAL=%d FULL=%d writer{%s} reader{%s} checkpoint{%s} busy=%d; fixture only, no API certification", policy.mode, policy.wait, elapsed, peakWAL, full, stats(writes), stats(reads), stats(checkpoints), busyCount)
		})
	}
}
