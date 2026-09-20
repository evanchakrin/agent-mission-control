package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrHistoryChanged = errors.New("history changed; restart pagination from the first page")

// A cursor binds to the interpretation and restore epoch, not to an organization
// revision or an append offset. Archive edits and appended records remain live.
func (s *Store) historySnapshot(ctx context.Context, id, expected string) (string, error) {
	var source, generation, revision string
	err := s.db.QueryRowContext(ctx, `SELECT source_id,generation,COALESCE(json_extract(projection,'$.projectionRevision'),'') FROM sessions WHERE id=?`, id).Scan(&source, &generation, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	actual := s.eventSnapshot(id, source, generation, revision)
	if expected != "" && expected != actual {
		return "", ErrHistoryChanged
	}
	return actual, nil
}

func (s *Store) eventSnapshot(id, source, generation, revision string) string {
	b, _ := json.Marshal([]string{s.epoch, id, source, generation, revision})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

func (s *Store) ListEventsPinned(ctx context.Context, id string, after int64, limit int, snapshot string) (EventPage, error) {
	if after < 0 || (after > 0 && snapshot == "") {
		return EventPage{}, fmt.Errorf("%w: continued history requires its snapshot", ErrInvalid)
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return EventPage{}, err
	}
	page, err := s.ListEvents(ctx, id, after, limit)
	if err != nil {
		return EventPage{}, err
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return EventPage{}, err
	}
	page.Snapshot = token
	return page, nil
}

// Agent cursors bind to both the projection and exact agent identity. A client
// cannot accidentally continue one agent's page in another agent's history.
func (s *Store) ListAgentEventsPinned(ctx context.Context, id, agent string, after int64, limit int, snapshot string) (EventPage, error) {
	if len(agent) > 4096 || after < 0 || (after > 0 && snapshot == "") {
		return EventPage{}, ErrInvalid
	}
	base, err := s.historySnapshot(ctx, id, "")
	if err != nil {
		return EventPage{}, err
	}
	data, _ := json.Marshal([]string{base, "agent-events", agent})
	hash := sha256.Sum256(data)
	token := hex.EncodeToString(hash[:])
	if snapshot != "" && snapshot != token {
		return EventPage{}, ErrHistoryChanged
	}
	page, err := s.queryEvents(ctx, `e.session_id=? AND e.agent_id=? AND e.seq>?`, []any{id, agent, after}, limit, false)
	if err != nil {
		return EventPage{}, err
	}
	if _, err = s.historySnapshot(ctx, id, base); err != nil {
		return EventPage{}, err
	}
	page.Snapshot = token
	for i := range page.Events {
		page.Events[i].HistorySnapshot = base
	}
	return page, nil
}

type UsagePage struct {
	Observations []UsageObservation `json:"observations"`
	NextCursor   string             `json:"nextCursor,omitempty"`
	Snapshot     string             `json:"snapshot"`
}

func (s *Store) GetUsagePinned(ctx context.Context, id, after string, limit int, snapshot string) (UsagePage, error) {
	if after != "" && snapshot == "" {
		return UsagePage{}, fmt.Errorf("%w: continued usage requires its snapshot", ErrInvalid)
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return UsagePage{}, err
	}
	rows, err := s.GetUsage(ctx, id, after, limit)
	if err != nil {
		return UsagePage{}, err
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return UsagePage{}, err
	}
	page := UsagePage{Observations: rows, Snapshot: token}
	if len(rows) == pageLimit(limit) {
		page.NextCursor = rows[len(rows)-1].ID
	}
	return page, nil
}

func (s *Store) EventsAroundPinned(ctx context.Context, id string, anchor int64, before, after int, snapshot string) (EventsWindow, error) {
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return EventsWindow{}, err
	}
	window, readErr := s.eventsAround(ctx, id, anchor, before, after)
	// Check even when the anchor vanished during publication, so the client
	// receives an actionable restart rather than an unexplained missing record.
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return EventsWindow{}, err
	}
	if readErr != nil {
		return EventsWindow{}, readErr
	}
	window.Snapshot = token
	return window, nil
}
