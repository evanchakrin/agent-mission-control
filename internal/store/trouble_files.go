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

type TroubleFile struct {
	MachineID       string   `json:"machineId"`
	Project         string   `json:"project"`
	Path            string   `json:"path"`
	Sessions        int64    `json:"sessions"`
	BadSessions     int64    `json:"badSessions"`
	UnknownSessions int64    `json:"unknownSessions"`
	LastTouched     string   `json:"lastTouched"`
	Rate            *float64 `json:"rate"`
}
type TroubleFilePage struct {
	Files           []TroubleFile `json:"files"`
	Snapshot        string        `json:"snapshot"`
	ObservedAt      time.Time     `json:"observedAt"`
	MinSessions     int           `json:"minSessions"`
	TotalSessions   int64         `json:"totalSessions"`
	IndexedSessions int64         `json:"indexedSessions"`
	Sort            string        `json:"sort"`
	Direction       string        `json:"direction"`
	NextCursor      string        `json:"nextCursor,omitempty"`
}
type troubleCursor struct {
	Snapshot, Sort, Direction string
	Offset                    int64
	At                        time.Time
}

// Shared evidence computation. A retry marker can only arise from a recorded
// error, already included in failed sessions. Current stalls are separate from
// old quiet sources. An unknown result is not evidence of successful execution.
// Keep the shared event relation narrow: SQLite may materialize it, so carrying
// transcript text or full JSON here makes temporary work scale with body size.
// Pending sequences already come from current projections; fetch their timestamp
// directly by the events primary key instead of indexing the materialized rows.
// Each session has one current source/generation/revision. Include those fixed
// identities in call grouping to follow events_tool_match without another sort.
const troubleEvidenceSQL = `WITH trouble_scope AS NOT MATERIALIZED (SELECT * FROM query_sessions),` + troubleEvidenceBody

// A single file's drilldown needs outcomes from its sessions, not every other
// session in the corpus. Keep the whole history of each matching session so an
// error or pending call on another file still contributes to that session.
const troubleFileEvidenceSQL = `WITH trouble_scope AS (
 SELECT q.* FROM query_sessions q JOIN sessions s ON s.id=q.id
 WHERE q.machine_id=? AND s.project=? AND EXISTS (
 SELECT 1 FROM file_edit_events f JOIN events e ON e.seq=f.event_seq
 WHERE e.session_id=q.id AND f.source_id=q.source_id AND f.generation=q.generation AND f.revision=q.projection_revision AND f.path=?)),` + troubleEvidenceBody

const troubleEvidenceBody = `current_events AS (
 SELECT e.seq,e.session_id,e.source_id,e.generation,e.projection_revision,e.agent_id,e.kind,json_extract(e.data,'$.toolUseId') AS call_id,json_type(e.data,'$.error') AS error_type FROM events e JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision),
 pairs AS (SELECT session_id,agent_id,call_id,
 SUM(kind='tool-call') AS calls,SUM(kind='tool-result') AS results,
 MAX(CASE WHEN kind='tool-call' THEN seq ELSE 0 END) AS call_seq,
 MAX(CASE WHEN kind='tool-result' THEN seq ELSE 0 END) AS result_seq
 FROM current_events WHERE kind IN ('tool-call','tool-result') GROUP BY session_id,source_id,generation,projection_revision,agent_id,call_id),
 pending AS (SELECT session_id,agent_id,MAX(CASE WHEN calls=1 AND results=0 AND typeof(call_id)='text' AND call_id<>'' AND agent_id<>'' THEN call_seq ELSE 0 END) AS call_seq,
 MAX(calls<>1 OR results>1 OR typeof(call_id)<>'text' OR call_id='' OR agent_id='' OR results=1 AND result_seq<=call_seq) AS uncertain
 FROM pairs GROUP BY session_id,agent_id),
 stalls AS (SELECT p.session_id,MAX(CASE WHEN p.call_seq>0 AND julianday(e.timestamp)<julianday(?)-2.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?)-10.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))<=julianday(?) THEN 1 ELSE 0 END) AS stalled,
 MAX(p.uncertain OR p.call_seq>0 AND (julianday(e.timestamp) IS NULL OR julianday(e.timestamp)>julianday(?) OR julianday(json_extract(so.meta_json,'$.modifiedAt')) IS NULL OR julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?))) AS uncertain
 FROM pending p JOIN trouble_scope q ON q.id=p.session_id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation LEFT JOIN events e ON e.seq=p.call_seq WHERE p.call_seq>0 OR p.uncertain=1 GROUP BY p.session_id),
 outcomes AS (SELECT session_id,MAX(error_type='true') AS failed,
 MAX(kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false')) AS uncertain FROM current_events WHERE error_type='true' OR kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false') GROUP BY session_id),
 touches AS (SELECT DISTINCT q.id AS session_id,q.machine_id,s.project,f.path,COALESCE(NULLIF(json_extract(so.meta_json,'$.modifiedAt'),''),q.last_activity) AS last_activity,
 CASE WHEN COALESCE(o.failed,0)=1 OR COALESCE(st.stalled,0)=1 THEN 1 ELSE 0 END AS bad,
 CASE WHEN COALESCE(o.uncertain,0)=1 OR COALESCE(st.uncertain,0)=1 OR c.indexed_offset IS NULL OR c.indexed_offset<>so.indexed_offset OR c.diagnostics>0 THEN 1 ELSE 0 END AS uncertain
 FROM file_edit_events f JOIN events e ON e.seq=f.event_seq
 JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=f.source_id AND q.generation=f.generation AND q.projection_revision=f.revision
 JOIN sessions s ON s.id=q.id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation
 LEFT JOIN file_edit_checkpoints c ON c.source_id=f.source_id AND c.generation=f.generation AND c.revision=f.revision
 LEFT JOIN outcomes o ON o.session_id=q.id LEFT JOIN stalls st ON st.session_id=q.id)
`

// Exact recorded paths are scoped by machine and recorded source project, not
// current organization. This does not resolve filesystem aliases or read files.
// Aggregate rows are streamed; only the requested page occupies Go memory.
func (s *Store) TroubleFiles(ctx context.Context, cursor string, limit int, now time.Time) (TroubleFilePage, error) {
	return s.TroubleFilesSorted(ctx, cursor, limit, now, "sessions", "desc")
}

func (s *Store) TroubleFilesSorted(ctx context.Context, cursor string, limit int, now time.Time, sort, direction string) (TroubleFilePage, error) {
	out := TroubleFilePage{Files: []TroubleFile{}, MinSessions: 5, ObservedAt: now.UTC(), Sort: sort, Direction: direction}
	order, ok := map[string]string{
		"path":            "path COLLATE BINARY",
		"sessions":        "COUNT(*)",
		"badSessions":     "SUM(bad)",
		"unknownSessions": "SUM(CASE WHEN uncertain=1 AND bad=0 THEN 1 ELSE 0 END)",
		"lastTouched":     "MAX(julianday(last_activity))",
		"rate":            "CASE WHEN COUNT(*)>=5 AND SUM(CASE WHEN uncertain=1 AND bad=0 THEN 1 ELSE 0 END)=0 THEN SUM(bad)*100.0/COUNT(*) ELSE -1 END",
	}[sort]
	if !ok || (direction != "asc" && direction != "desc") {
		return out, ErrInvalid
	}
	if limit < 1 || limit > 100 || len(cursor) > 16384 || now.IsZero() {
		return out, ErrInvalid
	}
	var cp troubleCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || cp.Offset < 1 || cp.At.IsZero() || len(cp.Snapshot) != 64 {
			return out, ErrInvalid
		}
		if cp.Sort != sort || cp.Direction != direction {
			return out, ErrHistoryChanged
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
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(c.indexed_offset=so.indexed_offset AND c.diagnostics=0),0) FROM query_sessions q LEFT JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation LEFT JOIN file_edit_checkpoints c ON c.source_id=q.source_id AND c.generation=q.generation AND c.revision=q.projection_revision`).Scan(&out.TotalSessions, &out.IndexedSessions); err != nil {
		return out, err
	}
	// Only fixed expressions and validated direction are concatenated into SQL.
	rows, err := tx.QueryContext(ctx, troubleEvidenceSQL+`SELECT machine_id,project,path,COUNT(*),SUM(bad),SUM(CASE WHEN uncertain=1 AND bad=0 THEN 1 ELSE 0 END),strftime('%Y-%m-%dT%H:%M:%fZ',MAX(julianday(last_activity))) FROM touches GROUP BY machine_id,project,path ORDER BY `+order+` `+direction+`,machine_id COLLATE BINARY,project COLLATE BINARY,path COLLATE BINARY`, at, at, at, at, at)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]string{"trouble-files-v2", base, at, sort, direction})
	_ = enc.Encode([]int64{out.TotalSessions, out.IndexedSessions})
	more := false
	var position int64
	for rows.Next() {
		var f TroubleFile
		if err = rows.Scan(&f.MachineID, &f.Project, &f.Path, &f.Sessions, &f.BadSessions, &f.UnknownSessions, &f.LastTouched); err != nil {
			rows.Close()
			return out, err
		}
		if f.Sessions >= 5 && f.UnknownSessions == 0 {
			rate := float64(f.BadSessions) * 100 / float64(f.Sessions)
			f.Rate = &rate
		}
		_ = enc.Encode(f)
		position++
		if position <= cp.Offset {
			continue
		}
		if len(out.Files) < limit {
			out.Files = append(out.Files, f)
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
	if cursor != "" && cp.Snapshot != out.Snapshot {
		return TroubleFilePage{}, ErrHistoryChanged
	}
	if _, err = s.searchSnapshot(ctx, SearchQuery{}, base); err != nil {
		return TroubleFilePage{}, err
	}
	if more {
		raw, _ := json.Marshal(troubleCursor{out.Snapshot, sort, direction, cp.Offset + int64(len(out.Files)), out.ObservedAt})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
