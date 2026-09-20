package store

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// Metadata-only workload: does not create raw evidence or certify fleet/storage.
func TestSourceCatalog100001RetainedSources(t *testing.T) {
	runSourceCatalogScale(t, false)
}

func TestSourceCatalogLargeProjectionTitleSearch(t *testing.T) {
	runSourceCatalogScale(t, true)
}

func runSourceCatalogScale(t *testing.T, padded bool) {
	if testing.Short() {
		t.Skip("100001 retained-source catalog")
	}
	s := openTestStore(t, Options{})
	ctx := context.Background()
	start := time.Now()
	for _, sql := range []string{
		`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<100024)
 INSERT INTO source_identity SELECT printf('s%06d',x),CASE WHEN x<=100000 THEN 'm0' ELSE 'm'||(x-100000) END,'codex','','g1' FROM n`,
		`INSERT INTO sources(source_id,generation,meta_json,created_at,updated_at)
 SELECT source_id,'g1',json_object('machineId',machine_id,'sourceId',source_id,'generation','g1','provider','codex','path',CASE WHEN source_id='s100000' THEN 'rare-final-transcript.jsonl' ELSE source_id||'.jsonl' END,'size',0),'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM source_identity`,
	} {
		if _, err := s.db.ExecContext(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seed100025Sources25Machines=%s", time.Since(start))
	var machineReads []time.Duration
	for range 12 {
		start = time.Now()
		inventory, err := s.CatalogMachines(ctx, SessionQuery{Limit: 100})
		machineReads = append(machineReads, time.Since(start))
		if err != nil || len(inventory.Items) != 25 || inventory.NextCursor != "" {
			t.Fatal("source-only fleet omitted", inventory, err)
		}
		for _, item := range inventory.Items {
			if !item.SourceOnly || item.Sessions != 0 || item.Accounting != nil || item.CostEstimate != nil {
				t.Fatal("invented fleet accounting", item)
			}
		}
	}
	sort.Slice(machineReads, func(i, j int) bool { return machineReads[i] < machineReads[j] })
	machineP95 := machineReads[len(machineReads)-1]
	t.Logf("sourceOnlyMachineInventory p95 (12 samples)=%s", machineP95)
	if machineP95 > 200*time.Millisecond {
		t.Errorf("source-only inventory exceeded 200ms")
	}
	var pages, searches []time.Duration
	for range 12 {
		start = time.Now()
		p, err := s.SourceCatalog(ctx, "m0", "", 100)
		pages = append(pages, time.Since(start))
		if err != nil || len(p.Items) != 100 || p.NextCursor == "" {
			t.Fatal(p, err)
		}
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		start = time.Now()
		p, err = s.SearchSourceCatalog(request, "m0", "", 100, "rare-final")
		searches = append(searches, time.Since(start))
		cancel()
		if err != nil || len(p.Items) != 1 || p.Items[0].Source.SourceID != "s100000" || p.NextCursor != "" {
			t.Fatal(p, err)
		}
	}
	for _, sample := range []struct {
		name   string
		values []time.Duration
		limit  time.Duration
	}{{"page", pages, 200 * time.Millisecond}, {"last-path-match", searches, 500 * time.Millisecond}} {
		sort.Slice(sample.values, func(i, j int) bool { return sample.values[i] < sample.values[j] })
		p95 := sample.values[len(sample.values)-1]
		t.Logf("%s p95 (12 samples)=%s", sample.name, p95)
		if p95 > sample.limit {
			t.Errorf("%s exceeded %s", sample.name, sample.limit)
		}
	}
	start = time.Now()
	cursor, last := "", ""
	count := 0
	for {
		p, err := s.SourceCatalog(ctx, "m0", cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range p.Items {
			if item.Source.MachineID != "m0" || item.Source.SourceID <= last || item.SessionID != "" {
				t.Fatal("cross-machine, duplicate or invented projection", item)
			}
			last = item.Source.SourceID
			count++
		}
		if p.NextCursor == "" {
			break
		}
		if p.NextCursor == cursor {
			t.Fatal("cursor stalled")
		}
		cursor = p.NextCursor
	}
	if count != 100001 {
		t.Fatal("source catalog truncated", count)
	}
	t.Logf("fullBoundedTraversal=%s count=%d", time.Since(start), count)
	// Add minimal indexed projections without pretending these are parsed raw
	// transcripts. The terminal title cannot be found by the recorded-path branch.
	padding := ""
	if padded {
		padding = strings.Repeat("x", 4096)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions SELECT 'session-'||source_id,source_id,'g1',machine_id,provider,'','2026-01-01T00:00:00Z',json_object('title',CASE WHEN source_id='s100000' THEN 'Unique indexed title' ELSE 'Ordinary title' END,'fixturePadding',?) FROM source_identity WHERE machine_id='m0'`, padding); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"indexed", "owner"} {
		text := "unique indexed"
		if mode == "owner" {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO session_metadata VALUES('session-s100000',1,'{"name":"Unique owner name","archived":true}')`); err != nil {
				t.Fatal(err)
			}
			text = "unique owner"
		}
		var samples []time.Duration
		for range 12 {
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			start = time.Now()
			p, err := s.SearchSourceCatalog(request, "m0", "", 100, text)
			samples = append(samples, time.Since(start))
			cancel()
			if err != nil || len(p.Items) != 1 || p.Items[0].SessionID != "session-s100000" || p.NextCursor != "" {
				t.Fatal(mode, p, err)
			}
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		p95 := samples[len(samples)-1]
		t.Logf("%s-title p95 (12 samples)=%s", mode, p95)
		if p95 > 500*time.Millisecond {
			t.Errorf("%s-title exceeded 500ms", mode)
		}
	}
	var mixedReads []time.Duration
	for range 12 {
		start = time.Now()
		inventory, err := s.CatalogMachines(ctx, SessionQuery{Limit: 100})
		mixedReads = append(mixedReads, time.Since(start))
		if err != nil || len(inventory.Items) != 25 || inventory.NextCursor != "" {
			t.Fatal("mixed fleet omitted", inventory, err)
		}
		for _, item := range inventory.Items {
			if item.ID == "m0" {
				if item.SourceOnly || item.Sessions != 100001 {
					t.Fatal("indexed inventory changed", item)
				}
			} else if !item.SourceOnly || item.Sessions != 0 {
				t.Fatal("source-only inventory changed", item)
			}
		}
	}
	sort.Slice(mixedReads, func(i, j int) bool { return mixedReads[i] < mixedReads[j] })
	mixedP95 := mixedReads[len(mixedReads)-1]
	t.Logf("mixedMachineInventory p95 (12 samples)=%s", mixedP95)
	if mixedP95 > 200*time.Millisecond {
		t.Errorf("mixed inventory exceeded 200ms")
	}
}
