package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func (s *Store) setupSearchCursors() error {
	_, err := s.db.Exec(`
INSERT OR IGNORE INTO properties(key,value) VALUES('search_revision','0');
CREATE TRIGGER IF NOT EXISTS search_interpretation_changed AFTER UPDATE ON sessions
WHEN OLD.source_id<>NEW.source_id OR OLD.generation<>NEW.generation
 OR COALESCE(json_extract(OLD.projection,'$.projectionRevision'),'')<>COALESCE(json_extract(NEW.projection,'$.projectionRevision'),'')
BEGIN UPDATE properties SET value=CAST(value AS INTEGER)+1 WHERE key='search_revision'; END;
CREATE TRIGGER IF NOT EXISTS search_session_removed AFTER DELETE ON sessions
BEGIN UPDATE properties SET value=CAST(value AS INTEGER)+1 WHERE key='search_revision'; END;
`)
	return err
}

func (s *Store) searchSnapshot(ctx context.Context, q SearchQuery, expected string) (string, error) {
	var revision int64
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='search_revision'").Scan(&revision); err != nil {
		return "", err
	}
	b, _ := json.Marshal([]any{s.epoch, revision, q.Text, q.SessionID})
	if q.DelegatedOnly {
		b, _ = json.Marshal([]any{s.epoch, revision, q.Text, q.SessionID, "delegated-only-v1"})
	}
	if q.Related {
		b, _ = json.Marshal([]any{s.epoch, revision, q.Text, q.SessionID, q.DelegatedOnly, "related-top-five-v1"})
	}
	hash := sha256.Sum256(b)
	actual := hex.EncodeToString(hash[:])
	if expected != "" && expected != actual {
		return "", ErrHistoryChanged
	}
	return actual, nil
}

// SearchPinned binds interpretation, query and restore epoch, but permits newly
// appended events. Checks surround a short query, never a long reader transaction.
func (s *Store) SearchPinned(ctx context.Context, q SearchQuery, snapshot string) (EventPage, error) {
	if q.AfterSequence < 0 || (q.AfterSequence > 0 && snapshot == "") {
		return EventPage{}, ErrInvalid
	}
	token, err := s.searchSnapshot(ctx, q, snapshot)
	if err != nil {
		return EventPage{}, err
	}
	page, err := s.Search(ctx, q)
	if err != nil {
		return EventPage{}, err
	}
	if q.DelegatedOnly {
		for i := range page.Events {
			call := &page.Events[i]
			spans, e := s.ToolSpans(ctx, call.SessionID, call.Sequence-1, 1, call.HistorySnapshot)
			if e != nil {
				return EventPage{}, e
			}
			if len(spans.Spans) != 1 || spans.Spans[0].Call.Sequence != call.Sequence || spans.Spans[0].Call.ID != call.ID {
				return EventPage{}, ErrHistoryChanged
			}
			span := spans.Spans[0]
			call.ToolResult = &ToolResultEvidence{State: span.State, ResultSequence: span.ResultSequence, Error: span.Error}
		}
	}
	if _, err = s.searchSnapshot(ctx, q, token); err != nil {
		return EventPage{}, err
	}
	page.Snapshot = token
	return page, nil
}
