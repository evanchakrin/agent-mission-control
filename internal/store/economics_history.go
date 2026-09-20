package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

type EconomicsHistoryEntry struct {
	ID          string               `json:"id"`
	Reason      string               `json:"reason"`
	Measurement EconomicsMeasurement `json:"measurement"`
}
type EconomicsHistoryPage struct {
	Items      []EconomicsHistoryEntry `json:"items"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

// CaptureEconomics samples all indexed history, including archived sessions.
// It never rewrites older measurements after pricing or parser changes. Reusing
// an operation ID returns the original measurement, not a fresh observation.
// A timer call within twenty hours of the latest capture returns nil.
func (s *Store) CaptureEconomics(ctx context.Context, id, reason string) (*EconomicsHistoryEntry, error) {
	return s.captureEconomics(ctx, id, reason, s.RecoveryEpoch())
}

// CaptureEconomicsAtEpoch permits receipt recovery across restores, but never
// substitutes a new measurement for an operation absent from restored history.
func (s *Store) CaptureEconomicsAtEpoch(ctx context.Context, id, epoch string) (*EconomicsHistoryEntry, error) {
	return s.captureEconomics(ctx, id, "manual", epoch)
}

func (s *Store) captureEconomics(ctx context.Context, id, reason, epoch string) (*EconomicsHistoryEntry, error) {
	if !validID(id) || reason != "manual" && reason != "timer" {
		return nil, ErrInvalid
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return nil, err
	}
	if prior, err := readEconomicsEntry(ctx, s.db, id, reason); err != nil || prior != nil {
		return prior, err
	}
	if epoch == "" || epoch != s.RecoveryEpoch() {
		return nil, ErrHistoryChanged
	}
	if resolved, e := readEconomicsResolution(ctx, s.db, id); e != nil || resolved != nil {
		if e != nil {
			return nil, e
		}
		return nil, ErrConflict
	}
	if reason == "timer" {
		var last sql.NullInt64
		if err := s.db.QueryRowContext(ctx, "SELECT MAX(measured_at) FROM economics_history").Scan(&last); err != nil {
			return nil, err
		}
		if last.Valid && time.Since(time.UnixMilli(last.Int64)) < 20*time.Hour {
			return nil, nil
		}
	}
	measurement, err := s.MeasureEconomics(ctx, SessionQuery{})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(measurement)
	if err != nil {
		return nil, err
	}
	// Aggregate before acquiring the application writer; hold it only to check
	// races and append this bounded eight-bucket measurement transactionally.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if prior, e := readEconomicsEntry(ctx, tx, id, reason); e != nil || prior != nil {
		return prior, e
	}
	if resolved, e := readEconomicsResolution(ctx, tx, id); e != nil || resolved != nil {
		if e != nil {
			return nil, e
		}
		return nil, ErrConflict
	}
	if reason == "timer" {
		var last sql.NullInt64
		if err = tx.QueryRowContext(ctx, "SELECT MAX(measured_at) FROM economics_history").Scan(&last); err != nil {
			return nil, err
		}
		if last.Valid && measurement.MeasuredAt.Sub(time.UnixMilli(last.Int64)) < 20*time.Hour {
			return nil, nil
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO economics_history(id,reason,measured_at,measurement) VALUES(?,?,?,?)", id, reason, measurement.MeasuredAt.UnixMilli(), raw); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &EconomicsHistoryEntry{ID: id, Reason: reason, Measurement: measurement}, nil
}

func readEconomicsEntry(ctx context.Context, db economicsReader, id, reason string) (*EconomicsHistoryEntry, error) {
	var entry EconomicsHistoryEntry
	var raw []byte
	err := db.QueryRowContext(ctx, "SELECT id,reason,measurement FROM economics_history WHERE id=?", id).Scan(&entry.ID, &entry.Reason, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry.Reason != reason {
		return nil, ErrConflict
	}
	if err = json.Unmarshal(raw, &entry.Measurement); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (s *Store) EconomicsHistory(ctx context.Context, cursor string, limit int) (EconomicsHistoryPage, error) {
	page := EconomicsHistoryPage{Items: []EconomicsHistoryEntry{}}
	if err := s.InitializeAccounting(ctx); err != nil {
		return page, err
	}
	after, has, err := decodeKey(cursor, "economics-history")
	if err != nil {
		return page, err
	}
	boundary := int64(1<<63 - 1)
	if has {
		parts := strings.SplitN(after, "|", 2)
		// A row number alone cannot identify history after a backup restore.
		// Reject old-format cursors too; restarting the page is lossless.
		if len(parts) != 2 || parts[1] != s.RecoveryEpoch() {
			return page, ErrHistoryChanged
		}
		boundary, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || boundary <= 0 {
			return page, ErrInvalid
		}
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, "SELECT seq,id,reason,measurement FROM economics_history WHERE seq<? ORDER BY seq DESC LIMIT ?", boundary, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var seq int64
		var entry EconomicsHistoryEntry
		var raw []byte
		if err = rows.Scan(&seq, &entry.ID, &entry.Reason, &raw); err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			page.NextCursor = encodeKey(strconv.FormatInt(last, 10)+"|"+s.RecoveryEpoch(), "economics-history")
			break
		}
		if err = json.Unmarshal(raw, &entry.Measurement); err != nil {
			return page, err
		}
		page.Items = append(page.Items, entry)
		last = seq
	}
	return page, rows.Err()
}
