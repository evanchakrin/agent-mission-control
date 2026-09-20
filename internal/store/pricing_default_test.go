package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestComparisonDefaultOnlyNewSessions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.InitializePricingQueue(ctx); err != nil {
		t.Fatal(err)
	}
	seedAnalyticsCatalog(t, s, 1)
	if err = s.PutRateCatalog(ctx, "rate", []byte(`{"id":"rate","rates":[]}`)); err != nil {
		t.Fatal(err)
	}
	p := ComparisonDefault{CatalogID: "rate", Context: "test", ComparisonAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	if err = s.SetComparisonDefault(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PricingPolicy(ctx, "s-000000"); !errors.Is(err, ErrNotFound) {
		t.Fatal("default rewrote existing session", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.ComparisonDefault(ctx)
	if err != nil || restored != p {
		t.Fatal("default not durable", restored, err)
	}
	insert := `INSERT INTO sessions SELECT ?,source_id,generation,machine_id,provider,project,last_activity,json_set(projection,'$.id',?) FROM sessions WHERE id='s-000000'`
	if _, err = s.db.ExecContext(ctx, insert, "new", "new"); err != nil {
		t.Fatal(err)
	}
	policy, err := s.PricingPolicy(ctx, "new")
	if err != nil || policy.CatalogID != p.CatalogID || policy.ComparisonAt == nil || !policy.ComparisonAt.Equal(p.ComparisonAt) || policy.ID == 0 {
		t.Fatal(policy, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, insert, "rollback", "rollback"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PricingPolicy(ctx, "rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rolled back policy survived", err)
	}
	if err = s.DisablePricingPolicy(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.title','updated') WHERE id='new'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PricingPolicy(ctx, "new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled policy reenabled", err)
	}
	if err = s.SetComparisonDefault(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, insert, "after-disable", "after-disable"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PricingPolicy(ctx, "after-disable"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cleared default still applied", err)
	}
}
