package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestRunSurvivesDatabaseContention(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x := Indexer{Store: s}
	done := make(chan error, 1)
	go func() { done <- x.Run(ctx) }()
	for x.Status().State != "blocked_storage" {
		select {
		case err := <-done:
			t.Fatalf("indexer exited on temporary writer contention: %v", err)
		case <-ctx.Done():
			t.Fatal("contention was not reported")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for x.Status().State != "caught-up" {
		select {
		case err := <-done:
			t.Fatalf("indexer failed to resume: %v", err)
		case <-ctx.Done():
			t.Fatal("indexer did not resume after writer released")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
