package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// Opt-in native I/O comparison, never a release gate or application setting.
// Two reader handles plus the writer/checkpoint pool total four connections.
func TestMappedTotalsUnderCheckpointDiagnostic(t *testing.T) {
	if testing.Short() || os.Getenv("AMC_MAPPED_CONTENTION_DIAGNOSTIC") != "1" {
		t.Skip("opt-in disposable mapped-read contention experiment")
	}
	// Keep the experiment explicit and bounded; this is not a service option.
	// Compare with the same seed-drain choice and workload in a separate run.
	checkpointInterval := time.Second
	switch os.Getenv("AMC_MAPPED_CONTENTION_INTERVAL_MS") {
	case "", "1000":
	case "100":
		checkpointInterval = 100 * time.Millisecond
	default:
		t.Fatal("AMC_MAPPED_CONTENTION_INTERVAL_MS must be 100 or 1000")
	}
	t.Logf("checkpoint interval=%s seedDrain=%t", checkpointInterval, os.Getenv("AMC_MAPPED_CONTENTION_DRAIN_SEED") == "1")
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 20000)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	expected, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TABLE mapped_pressure(id INTEGER PRIMARY KEY,data BLOB)`); err != nil {
		t.Fatal(err)
	}
	// Optional control isolates the seed's one large transaction from steady
	// checkpoint pressure. Report it separately; do not silently exclude it.
	if os.Getenv("AMC_MAPPED_CONTENTION_DRAIN_SEED") == "1" {
		started := time.Now()
		s.checkpointOnce(ctx)
		status := s.CheckpointStatus()
		t.Logf("seed checkpoint elapsed=%s status=%+v; excluded from following measured phase", time.Since(started), status)
		if status.State != "caught-up" || status.Problem != "" {
			t.Fatal("seed checkpoint did not complete", status)
		}
	}
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	type result struct {
		mapped  int
		samples []time.Duration
		err     error
	}
	results := make(chan result, 2)
	readers := []*sql.DB{}
	for _, mapped := range []int{0, 8 << 20} {
		dsn := "file:" + filepath.ToSlash(filepath.Join(s.dir, "ledger.sqlite")) + fmt.Sprintf("?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(%d)", mapped)
		db, e := sql.Open("sqlite", dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		var actual, readOnly int
		if e = db.QueryRowContext(ctx, `PRAGMA mmap_size`).Scan(&actual); e != nil || actual != mapped {
			t.Fatal(actual, e)
		}
		if e = db.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&readOnly); e != nil || readOnly != 1 {
			t.Fatal(readOnly, e)
		}
		readers = append(readers, db)
	}
	work, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Registered after the connection cleanup defers: stop and join first.
	defer func() { stop(); wg.Wait() }()
	for i, db := range readers {
		mapped := []int{0, 8 << 20}[i]
		wg.Add(1)
		go func(db *sql.DB, mapped int) {
			defer wg.Done()
			r := result{mapped: mapped}
			defer func() { results <- r }()
			reader := &Store{db: db}
			for work.Err() == nil {
				request, finish := context.WithTimeout(work, 5*time.Second)
				started := time.Now()
				totals, e := reader.CatalogTotals(request, SessionQuery{})
				elapsed := time.Since(started)
				finish()
				if work.Err() != nil {
					return
				}
				if e != nil {
					r.err = fmt.Errorf("read after %s: %w", elapsed, e)
					return
				}
				if !reflect.DeepEqual(expected, totals) {
					r.err = fmt.Errorf("recorded catalog changed")
					return
				}
				if len(r.samples) < 10000 {
					r.samples = append(r.samples, elapsed)
				}
				select {
				case <-work.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}(db, mapped)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(checkpointInterval)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
			}
			attempt, finish := context.WithTimeout(work, 5*time.Second)
			s.checkpointOnce(attempt)
			finish()
		}
	}()
	started := time.Now()
	for i := 0; i < 512; i++ {
		request, finish := context.WithTimeout(ctx, 5*time.Second)
		_, err = s.db.ExecContext(request, `INSERT INTO mapped_pressure(data) VALUES(zeroblob(65536))`)
		finish()
		if err != nil {
			t.Fatal("durable writer", i, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	wg.Wait()
	for range readers {
		r := <-results
		if r.err != nil {
			t.Error("mapped", r.mapped, r.err)
		}
		if len(r.samples) == 0 {
			t.Error("no complete reader samples", r.mapped)
			continue
		}
		sort.Slice(r.samples, func(i, j int) bool { return r.samples[i] < r.samples[j] })
		t.Logf("mapped=%d samples=%d p95=%s max=%s elapsed=%s; synthetic disk pressure, not fleet/API certification", r.mapped, len(r.samples), r.samples[(len(r.samples)*95+99)/100-1], r.samples[len(r.samples)-1], time.Since(started))
	}
	var count, full int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM mapped_pressure`).Scan(&count); err != nil || count != 512 {
		t.Fatal(count, err)
	}
	if err = s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
		t.Fatal("FULL durability", full, err)
	}
	if err = s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("writerRows=%d rawPressureBytes=%d checkpoint=%+v", count, 512<<16, s.CheckpointStatus())
}
