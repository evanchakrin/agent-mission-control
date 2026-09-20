package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type EconomicsResolutionPage struct {
	Items      []EconomicsCaptureResolution `json:"items"`
	NextCursor string                       `json:"nextCursor,omitempty"`
}

func (s *Store) EconomicsCaptureResolutions(ctx context.Context, cursor string, limit int) (EconomicsResolutionPage, error) {
	page := EconomicsResolutionPage{Items: []EconomicsCaptureResolution{}}
	if err := s.InitializeAccounting(ctx); err != nil {
		return page, err
	}
	key, has, err := decodeKey(cursor, "economics-resolutions")
	if err != nil {
		return page, err
	}
	var boundary struct{ At, ID, Epoch string }
	query := "SELECT id,original_epoch,resolved_epoch,resolved_at FROM economics_capture_resolutions"
	args := []any{}
	if has {
		if json.Unmarshal([]byte(key), &boundary) != nil || !validID(boundary.ID) {
			return page, ErrInvalid
		}
		if _, err = time.Parse(time.RFC3339Nano, boundary.At); err != nil {
			return page, ErrInvalid
		}
		if boundary.Epoch != s.RecoveryEpoch() {
			return page, ErrHistoryChanged
		}
		query += " WHERE (resolved_at,id)<(?,?)"
		args = append(args, boundary.At, boundary.ID)
	}
	limit = pageLimit(limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY resolved_at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var r EconomicsCaptureResolution
		r.Outcome = "capture-unavailable"
		if err = rows.Scan(&r.ID, &r.OriginalEpoch, &r.ResolvedEpoch, &r.ResolvedAt); err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			boundary.At = last.ResolvedAt
			boundary.ID = last.ID
			boundary.Epoch = s.RecoveryEpoch()
			raw, _ := json.Marshal(boundary)
			page.NextCursor = encodeKey(string(raw), "economics-resolutions")
			break
		}
		page.Items = append(page.Items, r)
	}
	return page, rows.Err()
}

// EconomicsCaptureResolution records an unavailable attempt, not a measurement
// and not proof that the original request ever committed. It is immutable and
// backed up with the ledger; resolving it never creates replacement usage.
type EconomicsCaptureResolution struct {
	ID            string `json:"id"`
	OriginalEpoch string `json:"originalEpoch"`
	ResolvedEpoch string `json:"resolvedEpoch"`
	ResolvedAt    string `json:"resolvedAt"`
	Outcome       string `json:"outcome"`
}

func readEconomicsResolution(ctx context.Context, db economicsReader, id string) (*EconomicsCaptureResolution, error) {
	r := EconomicsCaptureResolution{ID: id, Outcome: "capture-unavailable"}
	err := db.QueryRowContext(ctx, "SELECT original_epoch,resolved_epoch,resolved_at FROM economics_capture_resolutions WHERE id=?", id).Scan(&r.OriginalEpoch, &r.ResolvedEpoch, &r.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) EconomicsCaptureResolution(ctx context.Context, id string) (*EconomicsCaptureResolution, error) {
	if !validID(id) {
		return nil, ErrInvalid
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return nil, err
	}
	r, err := readEconomicsResolution(ctx, s.db, id)
	if err == nil && r == nil {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *Store) ResolveEconomicsCapture(ctx context.Context, id, originalEpoch, currentEpoch string) (*EconomicsCaptureResolution, error) {
	if !validID(id) || len(originalEpoch) > 128 || originalEpoch != "" && !validID(originalEpoch) {
		return nil, ErrInvalid
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if prior, e := readEconomicsResolution(ctx, tx, id); e != nil || prior != nil {
		if prior != nil && prior.OriginalEpoch != originalEpoch {
			return nil, ErrConflict
		}
		return prior, e
	}
	if currentEpoch == "" || currentEpoch != s.RecoveryEpoch() {
		return nil, ErrHistoryChanged
	}
	if originalEpoch == currentEpoch {
		return nil, ErrConflict
	}
	if receipt, e := readEconomicsEntry(ctx, tx, id, "manual"); e != nil || receipt != nil {
		if e != nil {
			return nil, e
		}
		return nil, ErrConflict // Recover the surviving receipt instead.
	}
	r := EconomicsCaptureResolution{ID: id, OriginalEpoch: originalEpoch, ResolvedEpoch: currentEpoch, ResolvedAt: stamp(time.Now()), Outcome: "capture-unavailable"}
	if _, err = tx.ExecContext(ctx, "INSERT INTO economics_capture_resolutions(id,original_epoch,resolved_epoch,resolved_at) VALUES(?,?,?,?)", id, originalEpoch, currentEpoch, r.ResolvedAt); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}
