package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

type UnsavedCandidate struct {
	MachineID        string `json:"machineId"`
	Project          string `json:"project"`
	Path             string `json:"path"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	SessionID        string `json:"sessionId"`
	LastTouched      string `json:"lastTouched"`
	Sessions         int64  `json:"sessions"`
}
type UnsavedCandidatePage struct {
	Files           []UnsavedCandidate `json:"files"`
	MachineID       string             `json:"machineId"`
	TotalFiles      int64              `json:"totalFiles"`
	TotalSessions   int64              `json:"totalSessions"`
	IndexedSessions int64              `json:"indexedSessions"`
	Snapshot        string             `json:"snapshot"`
	NextCursor      string             `json:"nextCursor,omitempty"`
}
type unsavedCandidateCursor struct {
	Snapshot string
	Offset   int64
}

// UnsavedCandidates is historical evidence, not a filesystem check. Archived
// sessions participate and only current source projections contribute. Exact
// paths stay scoped to their recorded machine and project; no remote path can
// grant the desktop local authority. Full results are streamed into the cursor
// hash while only the bounded page is retained in Go memory.
func (s *Store) UnsavedCandidates(ctx context.Context, machine, cursor string, limit int) (UnsavedCandidatePage, error) {
	out := UnsavedCandidatePage{Files: []UnsavedCandidate{}, MachineID: machine}
	if machine == "" || len(machine) > 1024 || len(cursor) > 8192 || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	var cp unsavedCandidateCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || len(cp.Snapshot) != 64 || cp.Offset < 1 {
			return out, ErrInvalid
		}
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
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(c.indexed_offset=so.indexed_offset AND c.diagnostics=0),0)
 FROM query_sessions q LEFT JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation
 LEFT JOIN file_edit_checkpoints c ON c.source_id=q.source_id AND c.generation=q.generation AND c.revision=q.projection_revision
 WHERE q.machine_id=?`, machine).Scan(&out.TotalSessions, &out.IndexedSessions)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `WITH evidence AS (
 SELECT q.id,q.machine_id,s.project,f.path,e.timestamp,
 CASE WHEN substr(f.path,1,1) IN ('/','\') OR substr(f.path,2,1)=':' THEN ''
 WHEN json_type(e.data,'$.workingDirectory')='text' THEN json_extract(e.data,'$.workingDirectory') ELSE '' END AS working_directory
 FROM query_sessions q JOIN sessions s ON s.id=q.id
 JOIN file_edit_events f ON f.source_id=q.source_id AND f.generation=q.generation AND f.revision=q.projection_revision
 JOIN events e ON e.seq=f.event_seq AND e.session_id=q.id WHERE q.machine_id=?), touches AS (
 SELECT id AS session_id,machine_id,project,path,working_directory,COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ',MAX(julianday(timestamp))),'') AS last_touched
 FROM evidence GROUP BY id,machine_id,project,path,working_directory),
 ranked AS (SELECT *,COUNT(*) OVER(PARTITION BY machine_id,project,path,working_directory) AS sessions,
 ROW_NUMBER() OVER(PARTITION BY machine_id,project,path,working_directory ORDER BY last_touched DESC,session_id COLLATE BINARY) AS position FROM touches)
 SELECT machine_id,project,path,working_directory,session_id,last_touched,sessions FROM ranked WHERE position=1
 ORDER BY last_touched DESC,project COLLATE BINARY,path COLLATE BINARY,working_directory COLLATE BINARY`, machine)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]string{"unsaved-candidates-v2", base, machine})
	_ = enc.Encode([]int64{out.TotalSessions, out.IndexedSessions})
	more := false
	for rows.Next() {
		var item UnsavedCandidate
		if err = rows.Scan(&item.MachineID, &item.Project, &item.Path, &item.WorkingDirectory, &item.SessionID, &item.LastTouched, &item.Sessions); err != nil {
			rows.Close()
			return out, err
		}
		out.TotalFiles++
		_ = enc.Encode(item)
		if out.TotalFiles <= cp.Offset {
			continue
		}
		if len(out.Files) < limit {
			out.Files = append(out.Files, item)
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
		return UnsavedCandidatePage{}, ErrHistoryChanged
	}
	if _, err = s.searchSnapshot(ctx, SearchQuery{}, base); err != nil {
		return UnsavedCandidatePage{}, err
	}
	if more {
		raw, _ := json.Marshal(unsavedCandidateCursor{out.Snapshot, cp.Offset + int64(len(out.Files))})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
