package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

// exportBoundary is stricter than a live history cursor: append, repricing,
// organization, reindex and restore all invalidate an in-progress export.
// Each read is short; exports never pin a WAL reader for their full duration.
func (s *Store) exportBoundary(ctx context.Context, id string) (string, error) {
	var projection, metadata, machineName string
	var durable, indexed, labelRevision int64
	err := s.db.QueryRowContext(ctx, `SELECT s.projection,COALESCE(m.value,''),COALESCE(r.durable_offset,-1),COALESCE(r.indexed_offset,-1),COALESCE(NULLIF(ml.display_name,''),NULLIF(json_extract(mh.heartbeat,'$.name'),''),s.machine_id),COALESCE(ml.revision,0)
 FROM sessions s LEFT JOIN session_metadata m ON m.session_id=s.id
 LEFT JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation
 LEFT JOIN machine_labels ml ON ml.machine_id=s.machine_id LEFT JOIN machines mh ON mh.machine_id=s.machine_id WHERE s.id=?`, id).Scan(&projection, &metadata, &durable, &indexed, &machineName, &labelRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal([]any{s.epoch, id, projection, metadata, durable, indexed, machineName, labelRevision})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type exportRecord struct {
	Type         string            `json:"type"`
	Version      int               `json:"version,omitempty"`
	Boundary     string            `json:"boundary,omitempty"`
	Description  string            `json:"description,omitempty"`
	Session      *Session          `json:"session,omitempty"`
	Event        *Event            `json:"event,omitempty"`
	Usage        *UsageObservation `json:"usage,omitempty"`
	Events       int64             `json:"events,omitempty"`
	Observations int64             `json:"observations,omitempty"`
}

// WriteIndexedExport writes versioned JSONL with a required final complete record.
// Errors, interrupted downloads and missing trailers are incomplete exports.
// Indexed text may be a preview; immutable source bytes remain a separate export.
func (s *Store) WriteIndexedExport(ctx context.Context, id string, w io.Writer) error {
	boundary, err := s.exportBoundary(ctx, id)
	if err != nil {
		return err
	}
	check := func() error {
		current, err := s.exportBoundary(ctx, id)
		if err != nil {
			return err
		}
		if current != boundary {
			return ErrHistoryChanged
		}
		return nil
	}
	session, err := s.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	if err = encoder.Encode(exportRecord{Type: "header", Version: 1, Boundary: boundary, Session: &session, Description: "Indexed history, not complete source bytes. Text may be abbreviated. Valid only with a final complete record carrying this boundary."}); err != nil {
		return err
	}
	var events, observations, after int64
	snapshot := ""
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		page, err := s.ListEventsPinned(ctx, id, after, 100, snapshot)
		if err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		snapshot = page.Snapshot
		for _, event := range page.Events {
			if err = encoder.Encode(exportRecord{Type: "event", Event: &event}); err != nil {
				return err
			}
			events++
		}
		if page.NextSequence == 0 {
			break
		}
		after = page.NextSequence
	}
	cursor := ""
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		page, err := s.GetUsagePinned(ctx, id, cursor, 100, snapshot)
		if err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		for _, usage := range page.Observations {
			if err = encoder.Encode(exportRecord{Type: "usage", Usage: &usage}); err != nil {
				return err
			}
			observations++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if err = check(); err != nil {
		return err
	}
	return encoder.Encode(exportRecord{Type: "complete", Version: 1, Boundary: boundary, Events: events, Observations: observations})
}
