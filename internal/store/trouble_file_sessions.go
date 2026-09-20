package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"
)

type TroubleSession struct {
	ID          string `json:"id"`
	LastTouched string `json:"lastTouched"`
	Bad         bool   `json:"bad"`
	Unknown     bool   `json:"unknown"`
}
type TroubleSessionPage struct {
	Sessions   []TroubleSession `json:"sessions"`
	Total      int64            `json:"total"`
	Snapshot   string           `json:"snapshot"`
	ObservedAt time.Time        `json:"observedAt"`
	NextCursor string           `json:"nextCursor,omitempty"`
}
type troubleSessionCursor struct {
	Last, ID, Snapshot string
	At                 time.Time
}

func (s *Store) TroubleFileSessions(ctx context.Context, machine, project, path, cursor string, limit int, now time.Time) (TroubleSessionPage, error) {
	out := TroubleSessionPage{Sessions: []TroubleSession{}, ObservedAt: now.UTC()}
	if machine == "" || len(machine) > 1024 || len(project) > 4096 || path == "" || len(path) > 4096 || len(cursor) > 8192 || limit < 1 || limit > 100 || now.IsZero() {
		return out, ErrInvalid
	}
	var cp troubleSessionCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || len(cp.Snapshot) != 64 || cp.ID == "" || len(cp.ID) > 1024 || len(cp.Last) > 128 || cp.At.IsZero() {
			return out, ErrInvalid
		}
		out.ObservedAt = cp.At
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	base, err := s.searchSnapshot(ctx, SearchQuery{}, "")
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	at := stamp(out.ObservedAt)
	// Match the file aggregate's millisecond UTC display. Fixed-width timestamps
	// keep SQL ordering and continuation comparisons consistent across source
	// timestamps with different fractional precision or timezone offsets.
	// Original timestamps remain in the raw evidence and event projections.
	rows, err := tx.QueryContext(ctx, troubleFileEvidenceSQL+`SELECT session_id,COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ',last_activity),'') AS touched,bad,CASE WHEN bad=0 THEN uncertain ELSE 0 END FROM touches WHERE machine_id=? AND project=? AND path=? ORDER BY touched DESC,session_id COLLATE BINARY`, machine, project, path, at, at, at, at, at, machine, project, path)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]string{"trouble-sessions-v2", base, at, machine, project, path})
	more := false
	for rows.Next() {
		var item TroubleSession
		if err = rows.Scan(&item.ID, &item.LastTouched, &item.Bad, &item.Unknown); err != nil {
			rows.Close()
			return out, err
		}
		out.Total++
		_ = enc.Encode(item)
		if cursor != "" && (item.LastTouched > cp.Last || item.LastTouched == cp.Last && item.ID <= cp.ID) {
			continue
		}
		if len(out.Sessions) < limit {
			out.Sessions = append(out.Sessions, item)
		} else {
			more = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.Snapshot = hex.EncodeToString(h.Sum(nil))
	if cursor != "" && out.Snapshot != cp.Snapshot {
		return TroubleSessionPage{}, ErrHistoryChanged
	}
	if _, err = s.searchSnapshot(ctx, SearchQuery{}, base); err != nil {
		return TroubleSessionPage{}, err
	}
	if more {
		last := out.Sessions[len(out.Sessions)-1]
		raw, _ := json.Marshal(troubleSessionCursor{last.LastTouched, last.ID, out.Snapshot, out.ObservedAt})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
