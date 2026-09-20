package store

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

// Opt-in native storage experiment, never a corpus/release certificate. Both
// modes use fresh private ledgers, FULL commits, the same writer/read workload,
// and the existing passive/reclamation logic. Only checkpoint cadence differs.
func TestCheckpointCadenceDiagnostic(t *testing.T) {
	if os.Getenv("AMC_CHECKPOINT_CADENCE_DIAGNOSTIC") != "1" || testing.Short() {
		t.Skip("opt-in native checkpoint cadence experiment")
	}
	for _, interval := range []time.Duration{10 * time.Second, time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			s := openTestStore(t, Options{ExternalCheckpointOwner: true})
			if _, err := s.db.Exec(`CREATE TABLE cadence_fixture(id INTEGER PRIMARY KEY,data BLOB)`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
			defer cancel()
			var workers sync.WaitGroup
			var readMS, checkpointMS []float64
			var readProblem error
			workers.Add(2)
			go func() {
				defer workers.Done()
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						read, stop := context.WithTimeout(ctx, 5*time.Second)
						started := time.Now()
						var maximum int
						err := s.db.QueryRowContext(read, `SELECT COALESCE(MAX(id),0) FROM cadence_fixture`).Scan(&maximum)
						stop()
						if ctx.Err() != nil {
							return
						}
						if err != nil {
							readProblem = err
							return
						}
						readMS = append(readMS, float64(time.Since(started))/float64(time.Millisecond))
					}
				}
			}()
			go func() {
				defer workers.Done()
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						checkpoint, stop := context.WithTimeout(ctx, 5*time.Second)
						started := time.Now()
						s.checkpointOnce(checkpoint)
						stop()
						checkpointMS = append(checkpointMS, float64(time.Since(started))/float64(time.Millisecond))
					}
				}
			}()
			writes := 0
			for ctx.Err() == nil {
				_, err := s.db.ExecContext(ctx, `INSERT INTO cadence_fixture(data) VALUES(zeroblob(16384))`)
				if err != nil {
					if ctx.Err() == nil {
						t.Error(err)
					}
					break
				}
				writes++
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Millisecond):
				}
			}
			cancel()
			workers.Wait()
			if readProblem != nil {
				t.Error("read failed", readProblem)
			}
			if len(readMS) < 100 || len(checkpointMS) == 0 {
				t.Fatalf("insufficient samples: reads %d checkpoints %d", len(readMS), len(checkpointMS))
			}
			sort.Float64s(readMS)
			sort.Float64s(checkpointMS)
			p95 := readMS[(len(readMS)*95+99)/100-1]
			t.Logf("cadence=%s writes=%d payloadBytes=%d reads=%d readP95MS=%.3f readMaxMS=%.3f checkpoints=%d checkpointMaxMS=%.3f", interval, writes, writes*16384, len(readMS), p95, readMS[len(readMS)-1], len(checkpointMS), checkpointMS[len(checkpointMS)-1])
			if p95 >= 200 {
				t.Error("diagnostic read p95 exceeded 200ms; not a session-page certification")
			}
		})
	}
}
