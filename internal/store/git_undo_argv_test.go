package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestArgvUndoEvidenceDurableAndRetrySafe(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 1)
	b.Events[0].Data = json.RawMessage(`{"tool":"functions.shell"}`)
	b.Events[0].SearchText = `{"command":["git","restore","--","$literal file.js"]}`
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitIndex(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatalf("already committed index range must reject retry: %v", err)
	}
	dir := s.dir
	s.Close()
	var err error
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil || page.Attempts != 1 || len(page.Events) != 1 || page.Unsupported != 0 {
		t.Fatal(page, err)
	}
	if page.Events[0].PathCount != 1 || page.Events[0].Paths[0] != "$literal file.js" {
		t.Fatal(page.Events)
	}
}
