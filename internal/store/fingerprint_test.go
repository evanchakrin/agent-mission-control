package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestFingerprintUsesCompleteScopedHistoryAndReportsMissingEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ingest(t, s, src, 0, "evidence\n")
	b := batch(src, 0, 9)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1001; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("call-%d", i), Kind: "tool-call", AgentID: "main", Timestamp: base.Add(time.Duration(i) * time.Second), SourceLength: 9})
	}
	b.Events = append(b.Events,
		Event{ID: "failure", Kind: "tool-result", Timestamp: base.Add(1000 * time.Second), Data: json.RawMessage(`{"error":true}`), SourceLength: 9},
		Event{ID: "undated", Kind: "tool-result", Data: json.RawMessage(`{"error":true}`), SourceLength: 9},
		Event{ID: "unknown", Kind: "tool-result", Timestamp: base, SourceLength: 9},
		Event{ID: "stale", Kind: "spawn", Timestamp: base.Add(-time.Hour), SourceLength: 9})
	if err := s.CommitIndex(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE events SET projection_revision='stale' WHERE id='stale'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionFingerprint(context.Background(), b.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var activity, failures int64
	for i := range got.Buckets {
		activity += got.Buckets[i]
		failures += got.Errors[i]
	}
	if got.Events != 1004 || activity != 1003 || failures != 1 || got.Errors[23] != 1 || got.UndatedEvents != 1 || got.UndatedActivity != 1 || got.UndatedErrors != 1 || got.UnknownResultStatus != 1 || got.DurationMS == nil || math.Abs(*got.DurationMS-1000000) > 1 || got.ThroughSequence == 0 {
		t.Fatalf("incomplete or mis-scoped fingerprint: %+v activity=%d failures=%d", got, activity, failures)
	}
}

func TestFingerprintEmptyAndSingleInstant(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ingest(t, s, src, 0, "evidence\n")
	b := batch(src, 0, 9)
	if err := s.CommitIndex(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionFingerprint(context.Background(), b.Session.ID)
	if err != nil || got.Events != 0 || got.DurationMS != nil {
		t.Fatal(got, err)
	}
	ingest(t, s, src, 9, "evidence\n")
	more := batch(src, 9, 18)
	more.Events = []Event{{ID: "instant", Kind: "spawn", Timestamp: time.Date(2026, 1, 1, 1, 0, 0, 0, time.FixedZone("offset", 3600)), SourceOffset: 9, SourceLength: 9}}
	if err = s.CommitIndex(context.Background(), more); err != nil {
		t.Fatal(err)
	}
	got, err = s.SessionFingerprint(context.Background(), b.Session.ID)
	if err != nil || got.Events != 1 || got.Buckets[0] != 1 || got.DurationMS == nil || *got.DurationMS != 0 {
		t.Fatal("single instant not represented", got, err)
	}
	if _, err = s.SessionFingerprint(context.Background(), "missing"); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestFingerprintTiersUseOnlySelectedAttributedHistoricalRates(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`INSERT INTO rate_catalogs VALUES('historical','now','{"rates":[{"id":"r","tier":"premium"}]}');
 INSERT INTO accounting_estimates VALUES('selected','s-000000','now','{"catalogId":"historical","attributionVersion":1}');
 INSERT INTO observation_prices VALUES('selected','usage-s-000000','main', 'model-0000','{"rateIds":["r"],"recordedTokens":4,"pricedTokens":2,"cost":0.5}');
 UPDATE sessions SET projection=json_set(projection,'$.pricing.snapshotId','selected') WHERE id='s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionFingerprint(ctx, "s-000000")
	if err != nil || got.RecordedTokens != 4 || len(got.Tiers) != 1 || got.Tiers[0].Tier != "premium" || got.Tiers[0].PricedTokens != 2 || got.Tiers[0].Cost != .5 {
		t.Fatal(got, err)
	}
	// A newer catalog is not a request to rewrite historical classification.
	if _, err = s.db.Exec(`INSERT INTO rate_catalogs VALUES('newer','later','{"rates":[{"id":"r","tier":"cheap"}]}')`); err != nil {
		t.Fatal(err)
	}
	got, err = s.SessionFingerprint(ctx, "s-000000")
	if err != nil || len(got.Tiers) != 1 || got.Tiers[0].Tier != "premium" {
		t.Fatal(got, err)
	}
	for _, update := range []string{`UPDATE observation_prices SET model='other'`, `UPDATE observation_prices SET model='model-0000',evidence=json_set(evidence,'$.recordedTokens',99)`, `UPDATE observation_prices SET evidence=json_set(evidence,'$.recordedTokens',4,'$.rateIds',json('["r","other"]'))`} {
		if _, err = s.db.Exec(update); err != nil {
			t.Fatal(err)
		}
		got, err = s.SessionFingerprint(ctx, "s-000000")
		if err != nil || len(got.Tiers) != 0 {
			t.Fatal("unsupported tier attribution", got, err)
		}
	}
}
