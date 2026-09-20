package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

func seedAnalyticsCatalog(t *testing.T, s *Store, count int) {
	t.Helper()
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sessionInsert, err := tx.PrepareContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionInsert.Close()
	usageInsert, err := tx.PrepareContext(ctx, "INSERT INTO usage_observations(id,session_id,source_id,generation,observation) VALUES(?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer usageInsert.Close()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("s-%06d", i)
		source := fmt.Sprintf("source-%06d", i)
		item := Session{ID: id, SourceID: source, Generation: "g1", MachineID: fmt.Sprintf("m%d", i%25), Provider: []string{"claude", "codex"}[i%2], Title: fmt.Sprintf("Transcript %06d", i), Project: fmt.Sprintf("p%03d", i%71), NativeID: id, LastActivity: base.Add(time.Duration(i) * time.Minute), TokensIn: 1, TokensCache: 1, TokensOut: 2, Completeness: "synthetic-fixture"}
		if i%3 == 0 {
			cost := float64(i%10) / 100
			item.CostEstimate = &cost
		}
		b, _ := json.Marshal(item)
		if _, err = sessionInsert.ExecContext(ctx, id, source, "g1", item.MachineID, item.Provider, item.Project, stamp(item.LastActivity), b); err != nil {
			t.Fatal(err)
		}
		u := UsageObservation{ID: "usage-" + id, SessionID: id, AgentID: "main", Model: fmt.Sprintf("model-%04d", i%1001), Timestamp: item.LastActivity, CounterScope: "provider-message", Kind: "message-final", TokensIn: 1, TokensCache: 1, TokensOut: 2}
		b, _ = json.Marshal(u)
		if _, err = usageInsert.ExecContext(ctx, u.ID, id, source, "g1", b); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsCatalog100001SessionsUsesWholeCorpusAndIndexedPages(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-session acceptance fixture")
	}
	s := openTestStore(t, Options{})
	seedStart := time.Now()
	seedAnalyticsCatalog(t, s, 100001)
	t.Logf("seed100001=%s", time.Since(seedStart))
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO session_metadata SELECT id,1,'{"pinned":true,"revision":1}' FROM sessions WHERE CAST(substr(id,3) AS INTEGER)%100=0`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	totals, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("whole-corpus totals=%s", time.Since(start))
	if totals.Sessions != 100001 || totals.TokensIn != 100001 || totals.TokensOut != 200002 {
		t.Fatalf("whole-corpus totals truncated: %+v", totals)
	}
	if totals.SessionsWithEstimate != 33334 || totals.SessionsWithoutEstimate != 66667 || totals.CostEstimate == nil {
		t.Fatalf("estimate coverage misleading: %+v", totals)
	}
	var timings []time.Duration
	for _, pinnedFirst := range []bool{false, true} {
		for _, sortKey := range []string{"lastActivity", "title", "tokens", "cost"} {
			q := SessionQuery{Sort: sortKey, Direction: "desc", Limit: 100, PinnedFirst: pinnedFirst}
			var sortTimings []time.Duration
			for i := 0; i < 12; i++ {
				start = time.Now()
				page, err := s.ListSessions(ctx, q)
				elapsed := time.Since(start)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Sessions) != 100 || page.NextCursor == "" {
					t.Fatalf("incomplete %s page", sortKey)
				}
				timings = append(timings, elapsed)
				sortTimings = append(sortTimings, elapsed)
				q.Cursor = page.NextCursor
			}
			sort.Slice(sortTimings, func(i, j int) bool { return sortTimings[i] < sortTimings[j] })
			t.Logf("sort=%s pinnedFirst=%v median=%s max=%s", sortKey, pinnedFirst, sortTimings[len(sortTimings)/2], sortTimings[len(sortTimings)-1])
		}
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	p95 := timings[(len(timings)*95-1)/100]
	t.Logf("page p95=%s max=%s across %d real SQL requests", p95, timings[len(timings)-1], len(timings))
	if p95 >= 200*time.Millisecond {
		t.Fatalf("page p95 %s exceeds200ms gate", p95)
	}
	start = time.Now()
	var allTokens, allSessions int64
	var groups int
	cursor := ""
	for {
		page, err := s.GroupedUsage(ctx, GroupQuery{SessionQuery: SessionQuery{Cursor: cursor, Limit: 500}, Dimension: "model"})
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range page.Groups {
			allTokens += group.TokensIn + group.TokensCache + group.TokensCacheWrite + group.TokensOut
			allSessions += group.Sessions
			groups++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	t.Logf("1001-model full-corpus histogram=%s", time.Since(start))
	if groups != 1001 || allTokens != 400004 || allSessions != 100001 {
		t.Fatalf("histogram lost records: groups=%d tokens=%d sessions=%d", groups, allTokens, allSessions)
	}
	catalog, err := s.CatalogMachines(ctx, SessionQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Items) != 25 {
		t.Fatalf("fleet catalog=%d, want25", len(catalog.Items))
	}
	var catalogCount int64
	for _, item := range catalog.Items {
		catalogCount += item.Sessions
	}
	if catalogCount != 100001 {
		t.Fatal("machine catalog was derived from a result page")
	}
	var fleetTimings []time.Duration
	for i := 0; i < 10; i++ {
		started := time.Now()
		page, e := s.CatalogMachines(ctx, SessionQuery{Limit: 100})
		fleetTimings = append(fleetTimings, time.Since(started))
		if e != nil {
			t.Fatal(e)
		}
		var tokens, awaiting int64
		for _, item := range page.Items {
			if item.Accounting == nil {
				t.Fatal("missing machine accounting")
			}
			tokens += item.Accounting.RecordedTokens
			awaiting += item.Accounting.TokensAwaitingPricing
		}
		if tokens != 400004 || awaiting != 400004 {
			t.Fatal("fleet accounting lost history or invented coverage", tokens, awaiting)
		}
	}
	sort.Slice(fleetTimings, func(i, j int) bool { return fleetTimings[i] < fleetTimings[j] })
	t.Logf("25-machine accounting over100001 sessions p95=%s", fleetTimings[9])
	if fleetTimings[9] >= 200*time.Millisecond {
		t.Fatal("fleet accounting page exceeds200ms", fleetTimings[9])
	}
	archived := true
	_, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{Archived: &archived, OperationID: "archive-catalog", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := s.CatalogTotals(ctx, SessionQuery{Archived: &archived})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Sessions != 1 {
		t.Fatalf("archive trigger did not refresh the relational filter: %+v", filtered)
	}
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	// Add a fully caught-up source catalog without filesystem IO. The pending
	// source poll must consult its partial index, not deserialize100k sources.
	_, err = s.db.ExecContext(ctx, `INSERT INTO source_identity SELECT source_id,machine_id,provider,native_id,generation FROM query_sessions;
	 INSERT INTO sources SELECT source_id,generation,json_object('sourceId',source_id,'machineId',machine_id,'provider',provider,'generation',generation),1,1,'{}',last_activity,last_activity FROM query_sessions;`)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	pending, err := s.PendingSources(ctx, "", 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("caught-up pending sources=%d err=%v", len(pending), err)
	}
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("caught-up pending-source scan=%s exceeds200ms", elapsed)
	} else {
		t.Logf("100001 caught-up source poll=%s", elapsed)
	}
	start = time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key)
	 SELECT 'catalog-event-'||id,id,source_id,generation,'main','assistant',last_activity,0,0,
	 CASE WHEN id='s-100000' THEN 'common captured history cosmic beacon' ELSE 'common captured history ordinary record' END,'{}','' FROM query_sessions;
	 INSERT INTO events_fts(rowid,text) SELECT seq,text FROM events;`)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("seed100001 searchable events=%s", time.Since(start))
	for _, text := range []string{"common", "cosmic", "cosmic beacon"} {
		var searchTimings []time.Duration
		for i := 0; i < 12; i++ {
			start = time.Now()
			page, err := s.SearchPinned(ctx, SearchQuery{Text: text, Limit: 100}, "")
			searchTimings = append(searchTimings, time.Since(start))
			if err != nil {
				t.Fatal(err)
			}
			if text == "common" && (len(page.Events) != 100 || page.NextSequence == 0) {
				t.Fatal("common-term first page did not retain full history continuation")
			}
			if text != "common" && (len(page.Events) != 1 || page.Events[0].SessionID != "s-100000") {
				t.Fatalf("rare/multiword search omitted tail history: %+v", page)
			}
		}
		sort.Slice(searchTimings, func(i, j int) bool { return searchTimings[i] < searchTimings[j] })
		p95 := searchTimings[(len(searchTimings)*95-1)/100]
		t.Logf("100001-event search=%q p95=%s", text, p95)
		if p95 >= 500*time.Millisecond {
			t.Fatalf("indexed search first-page p95=%s exceeds500ms", p95)
		}
	}
}

func TestExplicitProjectUnassignmentSurvivesReindexAndLegacyMigration(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	empty := ""
	meta, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{Project: &empty, OperationID: "unassign", Revision: 0})
	if err != nil || !meta.ProjectOverride {
		t.Fatalf("unassignment not durable: %+v %v", meta, err)
	}
	// A subsequent parser projection cannot reassign organization state.
	if _, err = s.db.ExecContext(ctx, `UPDATE sessions SET project='new-source-project' WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSessions(ctx, SessionQuery{UnassignedProject: true})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "s-000000" {
		t.Fatalf("unassignment lost after indexing: %+v %v", page, err)
	}
	// Old nonempty overrides keep their meaning; absent legacy project inherits.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO session_metadata VALUES('s-000001',1,'{"project":"old-assigned","revision":1}')`); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacyMetadata(ctx, map[string]string{"legacy": "s-000002"}, map[string]json.RawMessage{"legacy": json.RawMessage(`{"project":""}`)}); err != nil {
		t.Fatal(err)
	}
	page, err = s.ListSessions(ctx, SessionQuery{Project: "old-assigned"})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("old override changed: %+v %v", page, err)
	}
	// Simulate the previous read-model version; migration only rebuilds derived
	// tables, retaining the existing audit and explicit-override marker.
	if _, err = s.db.ExecContext(ctx, `UPDATE properties SET value='1' WHERE key='analytics_schema'`); err != nil {
		t.Fatal(err)
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = s.ListSessions(ctx, SessionQuery{UnassignedProject: true})
	if err != nil || len(page.Sessions) != 2 {
		t.Fatalf("migration lost explicit empty assignments: %+v %v", page, err)
	}
	if got, err := s.GetMetadata(ctx, "s-000000"); err != nil || got.Revision != 1 || !got.ProjectOverride {
		t.Fatalf("migration changed organization: %+v %v", got, err)
	}
}

func TestUsageCalendarBucketsAcrossYearAndWeekBoundaries(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 5)
	ctx := context.Background()
	for i, raw := range []string{"2025-12-28T23:59:59Z", "2025-12-29T00:00:00Z", "2026-01-04T23:59:59Z", "2026-01-05T00:00:00Z", "0001-01-01T00:00:00Z"} {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("s-%06d", i)
		u := UsageObservation{ID: "usage-" + id, SessionID: id, Timestamp: ts, TokensIn: 1}
		b, _ := json.Marshal(u)
		if _, err := s.db.ExecContext(ctx, `UPDATE usage_observations SET observation=? WHERE id=?`, b, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	for dimension, expected := range map[string][]string{"week": {"", "2025-12-22", "2025-12-29", "2026-01-05"}, "month": {"", "2025-12", "2026-01"}, "year": {"", "2025", "2026"}} {
		cursor := ""
		var keys []string
		var tokens int64
		for {
			page, err := s.GroupedUsage(ctx, GroupQuery{Dimension: dimension, SessionQuery: SessionQuery{Limit: 1, Cursor: cursor}})
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page.Groups {
				keys = append(keys, g.Key)
				tokens += g.TokensIn
				if g.Key == "" && g.Label != "Undated usage" {
					t.Fatalf("unknown timestamp: %+v", g)
				}
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = page.NextCursor
		}
		if fmt.Sprint(keys) != fmt.Sprint(expected) || tokens != 5 {
			t.Fatalf("%s: keys %v want %v, tokens %d", dimension, keys, expected, tokens)
		}
	}
}

func TestUsageCalendarNormalizesOffsetsAndNanosecondBounds(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	u := UsageObservation{ID: "usage-s-000000", SessionID: "s-000000", AgentID: "main", Model: "model", Timestamp: time.Date(2026, 9, 3, 22, 0, 0, 500000000, time.FixedZone("UTC-4", -4*60*60)), TokensIn: 7}
	b, _ := json.Marshal(u)
	if _, err := s.db.ExecContext(ctx, `UPDATE usage_observations SET observation=? WHERE id=?`, b, u.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.GroupedUsage(ctx, GroupQuery{Dimension: "day"})
	if err != nil || len(page.Groups) != 1 || page.Groups[0].Key != "2026-09-04" {
		t.Fatalf("usage day not UTC: %+v %v", page, err)
	}
	from := u.Timestamp.Add(time.Nanosecond)
	page, err = s.GroupedUsage(ctx, GroupQuery{Dimension: "day", SessionQuery: SessionQuery{From: &from}})
	if err != nil || len(page.Groups) != 0 {
		t.Fatalf("fractional time lower bound incorrect: %+v %v", page, err)
	}
	to := u.Timestamp
	page, err = s.GroupedUsage(ctx, GroupQuery{Dimension: "day", SessionQuery: SessionQuery{To: &to}})
	if err != nil || len(page.Groups) != 0 {
		t.Fatalf("exclusive upper bound incorrect: %+v %v", page, err)
	}
	stats, err := s.SessionStats(ctx, "s-000000")
	if err != nil || stats.FirstActivity == nil || !stats.FirstActivity.Equal(u.Timestamp) {
		t.Fatalf("usage-only time omitted: %+v %v", stats, err)
	}
}

func TestAnalyticsFiltersSortUnknownPricesAndRevisionTriggers(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 7)
	ctx := context.Background()
	name := "Renamed 100%_ history"
	project := "assigned"
	if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{Name: &name, Project: &project, OperationID: "rename", Revision: 0}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSessions(ctx, SessionQuery{Text: "100%_", Project: "assigned", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != "s-000001" {
		t.Fatalf("escaped title search or organization index incorrect: %+v", page)
	}
	var ids []string
	cursor := ""
	for {
		page, err = s.ListSessions(ctx, SessionQuery{Sort: "cost", Direction: "desc", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Sessions {
			ids = append(ids, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(ids) != 7 {
		t.Fatalf("cost sort hid unpriced sessions: %v", ids)
	}
	if ids[0] != "s-000006" || ids[1] != "s-000003" || ids[2] != "s-000000" {
		t.Fatalf("known estimates not ordered ahead of unknowns: %v", ids)
	}
	if _, err = s.ListSessions(ctx, SessionQuery{Sort: "DROP TABLE sessions"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("sort whitelist not enforced")
	}
	if _, err = s.ListSessions(ctx, SessionQuery{Sort: "title", Cursor: page.NextCursor + "invalid"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("malformed cursor accepted")
	}
	from := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)
	to := from.Add(2 * time.Minute)
	totals, err := s.CatalogTotals(ctx, SessionQuery{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Sessions != 2 {
		t.Fatalf("date filter not half-open: %+v", totals)
	}
}

func seedAgentHistory(t *testing.T, s *Store, count int) {
	t.Helper()
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO source_identity VALUES('scope-source','m1','claude','native-parent','g1')`)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	session := Session{ID: "scope-session", SourceID: "scope-source", Generation: "g1", MachineID: "m1", Provider: "claude", NativeID: "native-parent", Title: "Agents", LastActivity: base}
	b, _ := json.Marshal(session)
	if _, err = tx.Exec("INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?)", session.ID, session.SourceID, "g1", "m1", "claude", "", stamp(base), b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		agent := fmt.Sprintf("agent-%04d", i)
		kind := []string{"tool-call", "tool-result", "tool-result", "indexing-error"}[i%4]
		data := json.RawMessage(`{}`)
		if i%4 == 1 {
			if i%8 == 1 {
				data = json.RawMessage(`{"error":true}`)
			} else {
				data = json.RawMessage(`{"error":false}`)
			}
		}
		if _, err = tx.Exec(`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("event-%04d", i), session.ID, session.SourceID, "g1", agent, kind, stamp(base.Add(time.Duration(i)*time.Second)), 0, 1, "", data, ""); err != nil {
			t.Fatal(err)
		}
		model := fmt.Sprintf("model-%02d", i%65)
		if i%7 == 0 {
			model = ""
		}
		u := UsageObservation{ID: fmt.Sprintf("scope-usage-%04d", i), SessionID: session.ID, AgentID: agent, Model: model, Timestamp: base.Add(time.Duration(i) * time.Second), Kind: "message-final", TokensIn: 10, TokensOut: 2}
		b, _ := json.Marshal(u)
		if _, err = tx.Exec("INSERT INTO usage_observations(id,session_id,source_id,generation,observation) VALUES(?,?,?,?,?)", u.ID, session.ID, session.SourceID, "g1", b); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsAgentsModelsAndErrorsExceedOldCapsWithoutInventedAttribution(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAgentHistory(t, s, 2101)
	ctx := context.Background()
	statistics, err := s.SessionStats(ctx, "scope-session")
	if err != nil {
		t.Fatal(err)
	}
	if statistics.AgentCount != 2101 || statistics.Events != 2101 || statistics.ToolCalls != 526 || statistics.Errors != 263 || statistics.ToolResultsWithUnknownStatus != 525 || statistics.IndexingErrors != 525 {
		t.Fatalf("incorrect scopes/errors: %+v", statistics)
	}
	if statistics.DurationMS == nil || *statistics.DurationMS != 2100000 {
		t.Fatalf("incorrect duration: %+v", statistics)
	}
	agents := 0
	cursor := ""
	var unpriced int64
	for {
		page, err := s.SessionAgents(ctx, "scope-session", cursor, 500)
		if err != nil {
			t.Fatal(err)
		}
		agents += len(page.Agents)
		for _, agent := range page.Agents {
			unpriced += agent.UnknownModelTokens
			if agent.CostEstimate != nil {
				t.Fatal("agent costs invented without a price join")
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if agents != 2101 || unpriced != 301*12 {
		t.Fatalf("agents or unattributed usage truncated: %d %d", agents, unpriced)
	}
	models := 0
	tokens := int64(0)
	cursor = ""
	for {
		page, err := s.SessionModelUsage(ctx, "scope-session", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		models += len(page.Models)
		for _, model := range page.Models {
			tokens += model.TokensIn + model.TokensOut
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if models != 66 || tokens != 2101*12 {
		t.Fatalf("model history capped: models%d tokens%d", models, tokens)
	}
	// A changed provider observation replaces its derived contribution.
	var raw []byte
	if err = s.db.QueryRow("SELECT observation FROM usage_observations WHERE id='scope-usage-0000'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var u UsageObservation
	_ = json.Unmarshal(raw, &u)
	u.TokensIn = 30
	raw, _ = json.Marshal(u)
	if _, err = s.db.Exec("UPDATE usage_observations SET observation=? WHERE id=?", raw, u.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.SessionAgents(ctx, "scope-session", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Agents[0].TokensIn != 30 {
		t.Fatal("usage revision added or ignored instead of replacing")
	}
	if _, err = s.db.Exec("DELETE FROM events WHERE id='event-0000'"); err != nil {
		t.Fatal(err)
	}
	statistics, err = s.SessionStats(ctx, "scope-session")
	if err != nil {
		t.Fatal(err)
	}
	if statistics.Events != 2100 || statistics.ToolCalls != 525 || statistics.FirstActivity == nil {
		t.Fatalf("event projection did not reconcile deletion: %+v", statistics)
	}
}

func TestEventsAroundUsesSparseGlobalSequenceAndHandlesZeroSides(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAgentHistory(t, s, 9)
	ctx := context.Background()
	if _, err := s.db.Exec("DELETE FROM events WHERE seq IN(2,4,6,8)"); err != nil {
		t.Fatal(err)
	}
	window, err := s.EventsAround(ctx, "scope-session", 5, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Events) != 3 || window.Events[0].Sequence != 3 || window.Events[1].Sequence != 5 || window.Events[2].Sequence != 7 {
		t.Fatalf("wrong sparse window %+v", window)
	}
	zero, err := s.EventsAround(ctx, "scope-session", 5, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero.Events) != 1 || zero.Events[0].Sequence != 5 {
		t.Fatalf("zero-sized sides returned wrong event %+v", zero)
	}
	if _, err = s.EventsAround(ctx, "scope-session", 4, 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing sequence was treated as an array index")
	}
}

func TestAnalyticsBackfillAndReopenRetainOrganization(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "abc\n")
	b := batch(src, 0, 4)
	b.Events = []Event{{ID: "existing-tool", AgentID: "main", Kind: "tool-result", SourceOffset: 0, SourceLength: 4, Data: json.RawMessage(`{}`)}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := s.SessionStats(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolResultsWithUnknownStatus != 1 || stats.Errors != 0 {
		t.Fatalf("backfill invented an error status: %+v", stats)
	}
	archived := true
	if _, err = s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "persist-archive"}); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.ListSessions(ctx, SessionQuery{Archived: &archived})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || !page.Sessions[0].Metadata.Archived {
		t.Fatal("query schema reopen changed archive state")
	}
}
