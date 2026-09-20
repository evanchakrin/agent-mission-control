package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Revision  int64  `json:"revision"`
	Deleted   bool   `json:"deleted"`
	UpdatedAt string `json:"updatedAt"`
}

type ProjectMutation struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Color         string `json:"color"`
	Revision      int64  `json:"revision"`
	Delete        bool   `json:"delete"`
	OperationID   string `json:"operationId"`
	RecoveryEpoch string `json:"recoveryEpoch,omitempty"`
}

type ProjectPage struct {
	Items      []Project `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type ProjectAudit struct {
	OperationID string          `json:"operationId"`
	Revision    int64           `json:"revision"`
	Before      Project         `json:"before"`
	After       Project         `json:"after"`
	Request     json.RawMessage `json:"request"`
	At          string          `json:"at"`
}

type ProjectAuditPage struct {
	Items []ProjectAudit `json:"items"`
	Next  int64          `json:"next,omitempty"`
}

func (s *Store) ProjectHistory(ctx context.Context, id string, after int64, limit int) (ProjectAuditPage, error) {
	page := ProjectAuditPage{Items: []ProjectAudit{}}
	if !validID(id) || after < 0 {
		return page, ErrInvalid
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,revision,before_json,after_json,request_json,at FROM project_audit WHERE project_id=? AND revision>? ORDER BY revision LIMIT ?`, id, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var a ProjectAudit
		var before, current []byte
		if err = rows.Scan(&a.OperationID, &a.Revision, &before, &current, &a.Request, &a.At); err != nil {
			return page, err
		}
		if err = json.Unmarshal(before, &a.Before); err != nil {
			return page, err
		}
		if err = json.Unmarshal(current, &a.After); err != nil {
			return page, err
		}
		page.Items = append(page.Items, a)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[limit-1].Revision
	}
	return page, nil
}

type LegacyProject struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Original state.json remains in migration assets. Import seeds only missing
// registry identities; owner edits and deleted-ID tombstones always win.
func (s *Store) ImportLegacyProjects(ctx context.Context, projects []LegacyProject) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range projects {
		if !validID(p.ID) || strings.TrimSpace(p.Name) == "" || len(p.Name) > 1024 {
			return ErrInvalid
		}
		if p.Color == "" {
			p.Color = "#8a93a8"
		}
		if len(p.Color) != 7 || p.Color[0] != '#' {
			return ErrInvalid
		}
		if _, err = hex.DecodeString(p.Color[1:]); err != nil {
			return ErrInvalid
		}
		now := stamp(time.Now().UTC())
		project := Project{ID: p.ID, Name: p.Name, Color: p.Color, Revision: 1, UpdatedAt: now}
		res, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projects VALUES(?,?,?,?,?,?)`, p.ID, p.Name, p.Color, 1, false, now)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		after, _ := json.Marshal(project)
		request, _ := json.Marshal(p)
		if _, err = tx.ExecContext(ctx, "INSERT INTO project_audit VALUES(?,?,?,?,?,?,?)", "legacy-project-import:"+p.ID, p.ID, 1, []byte(`{}`), after, request, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes(kind,session_id,at) VALUES('project','',?)", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Projects(ctx context.Context, cursor string, limit int) (ProjectPage, error) {
	page := ProjectPage{Items: []Project{}}
	after, _, err := decodeKey(cursor, "projects")
	if err != nil {
		return page, err
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,color,revision,deleted,updated_at FROM projects WHERE deleted=0 AND id>? ORDER BY id LIMIT ?", after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Project
		if err = rows.Scan(&p.ID, &p.Name, &p.Color, &p.Revision, &p.Deleted, &p.UpdatedAt); err != nil {
			return page, err
		}
		page.Items = append(page.Items, p)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeKey(page.Items[limit-1].ID, "projects")
	}
	return page, nil
}

// Project names/colors are organization state, never provider-derived fields.
// Deleted IDs remain tombstones so stale assignments cannot recreate them.
func (s *Store) MutateProject(ctx context.Context, p ProjectMutation) (Project, error) {
	return s.mutateProject(ctx, p, false)
}

func (s *Store) mutateProject(ctx context.Context, p ProjectMutation, queued bool) (Project, error) {
	var empty Project
	if !validID(p.ID) || !validID(p.OperationID) || p.Revision < 0 || len(p.RecoveryEpoch) > 128 {
		return empty, ErrInvalid
	}
	if !p.Delete {
		if strings.TrimSpace(p.Name) == "" || len(p.Name) > 1024 || len(p.Color) != 7 || p.Color[0] != '#' {
			return empty, ErrInvalid
		}
		if _, err := hex.DecodeString(p.Color[1:]); err != nil {
			return empty, ErrInvalid
		}
	}
	request, err := json.Marshal(p)
	if err != nil {
		return empty, err
	}
	hash := sha256.Sum256(request)
	requestHash := hex.EncodeToString(hash[:])
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var priorHash string
	var result []byte
	err = tx.QueryRowContext(ctx, "SELECT request_hash,result FROM project_operations WHERE operation_id=?", p.OperationID).Scan(&priorHash, &result)
	if err == nil {
		if priorHash != requestHash {
			return empty, ErrConflict
		}
		err = json.Unmarshal(result, &empty)
		return empty, err
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	err = tx.QueryRowContext(ctx, "SELECT request_hash,result FROM project_deletions WHERE operation_id=?", p.OperationID).Scan(&priorHash, &result)
	if err == nil {
		if priorHash != requestHash || !queued {
			return empty, ErrConflict
		}
		err = json.Unmarshal(result, &empty)
		return empty, err
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	// A surviving receipt above is safe to replay after restore. An absent
	// operation from an earlier ledger epoch must never become a fresh write.
	if p.RecoveryEpoch != "" && p.RecoveryEpoch != s.epoch {
		return empty, ErrHistoryChanged
	}
	old := Project{ID: p.ID}
	err = tx.QueryRowContext(ctx, "SELECT name,color,revision,deleted,updated_at FROM projects WHERE id=?", p.ID).Scan(&old.Name, &old.Color, &old.Revision, &old.Deleted, &old.UpdatedAt)
	if err != nil && err != sql.ErrNoRows {
		return empty, err
	}
	if err == sql.ErrNoRows && p.Delete {
		return empty, ErrNotFound
	}
	if old.Revision != p.Revision || old.Deleted {
		return old, ErrConflict
	}
	now := stamp(time.Now().UTC())
	updated := Project{ID: p.ID, Name: p.Name, Color: p.Color, Revision: old.Revision + 1, Deleted: p.Delete, UpdatedAt: now}
	if p.Delete {
		updated.Name = old.Name
		updated.Color = old.Color
	}
	before, _ := json.Marshal(old)
	after, _ := json.Marshal(updated)
	if _, err = tx.ExecContext(ctx, `INSERT INTO projects VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,color=excluded.color,revision=excluded.revision,deleted=excluded.deleted,updated_at=excluded.updated_at`, updated.ID, updated.Name, updated.Color, updated.Revision, updated.Deleted, now); err != nil {
		return empty, err
	}
	if p.Delete && !queued {
		// SQL set operations avoid loading a corpus-sized membership list. Every
		// affected session advances its revision and keeps its own audit entry.
		where := ` WHERE json_extract(value,'$.project')=?`
		newJSON := `json_set(value,'$.project','','$.projectOverride',json('true'),'$.revision',revision+1,'$.updatedAt',?)`
		if _, err = tx.ExecContext(ctx, `INSERT INTO organization_audit SELECT ?||session_id,session_id,revision+1,value,`+newJSON+`,json_object('project','','revision',revision,'operationId',?||session_id),? FROM session_metadata`+where, "project-delete:"+p.OperationID+":", now, "project-delete:"+p.OperationID+":", now, p.ID); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) SELECT 'metadata',session_id,? FROM session_metadata`+where, now, p.ID); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE session_metadata SET revision=revision+1,value=`+newJSON+where, now, p.ID); err != nil {
			return empty, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO project_audit VALUES(?,?,?,?,?,?,?)", p.OperationID, p.ID, updated.Revision, before, after, request, now); err != nil {
		return empty, err
	}
	if queued {
		_, err = tx.ExecContext(ctx, "INSERT INTO project_deletions(operation_id,project_id,request_hash,result,state,updated_at) VALUES(?,?,?,?,'pending',?)", p.OperationID, p.ID, requestHash, after, now)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO project_operations VALUES(?,?,?)", p.OperationID, requestHash, after)
	}
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO changes(kind,session_id,at) VALUES('project','',?)", now); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, fmt.Errorf("project transaction: %w", err)
	}
	return updated, nil
}
