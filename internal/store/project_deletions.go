package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ProjectDeletion is durable progress, not an acknowledgement that all member
// sessions have changed. Only state=complete carries that acknowledgement.
type ProjectDeletion struct {
	OperationID string  `json:"operationId"`
	Project     Project `json:"project"`
	State       string  `json:"state"`
	Processed   int64   `json:"processed"`
	UpdatedAt   string  `json:"updatedAt"`
	Problem     string  `json:"problem,omitempty"`
}

func (s *Store) QueueProjectDeletion(ctx context.Context, p ProjectMutation) (ProjectDeletion, error) {
	if !p.Delete {
		return ProjectDeletion{}, ErrInvalid
	}
	project, err := s.mutateProject(ctx, p, true)
	if err != nil {
		return ProjectDeletion{}, err
	}
	job, err := s.ProjectDeletion(ctx, p.OperationID)
	if err == ErrNotFound {
		// A receipt from the synchronous protocol is also a completed deletion.
		return ProjectDeletion{OperationID: p.OperationID, Project: project, State: "complete", UpdatedAt: project.UpdatedAt}, nil
	}
	return job, err
}

func (s *Store) ProjectDeletion(ctx context.Context, operation string) (ProjectDeletion, error) {
	var job ProjectDeletion
	if !validID(operation) {
		return job, ErrInvalid
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT operation_id,result,state,processed,updated_at,problem FROM project_deletions WHERE operation_id=?`, operation).Scan(&job.OperationID, &raw, &job.State, &job.Processed, &job.UpdatedAt, &job.Problem)
	if err == sql.ErrNoRows {
		return job, ErrNotFound
	}
	if err != nil {
		return job, err
	}
	err = json.Unmarshal(raw, &job.Project)
	return job, err
}

// NextProjectDeletion schedules the least recently advanced job first. No
// historical list or membership list is retained in memory.
func (s *Store) NextProjectDeletion(ctx context.Context) (string, error) {
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return "", err
	}
	defer s.maintenanceMu.Unlock()
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT operation_id FROM project_deletions WHERE state='pending' ORDER BY updated_at,operation_id LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// AdvanceProjectDeletion commits at most 100 memberships with their individual
// audits. Latest metadata wins, including edits made after deletion acceptance.
func (s *Store) AdvanceProjectDeletion(ctx context.Context, operation string, limit int) (ProjectDeletion, error) {
	var job ProjectDeletion
	if !validID(operation) || limit < 1 || limit > 100 {
		return job, ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return job, err
	}
	defer tx.Rollback()
	var raw []byte
	var requestHash string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,result,state,processed,updated_at,request_hash FROM project_deletions WHERE operation_id=?`, operation).Scan(&job.OperationID, &raw, &job.State, &job.Processed, &job.UpdatedAt, &requestHash)
	if err == sql.ErrNoRows {
		return job, ErrNotFound
	}
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal(raw, &job.Project); err != nil {
		return job, err
	}
	if job.State == "complete" {
		return job, nil
	}
	var deleted bool
	if err = tx.QueryRowContext(ctx, `SELECT deleted FROM projects WHERE id=?`, job.Project.ID).Scan(&deleted); err != nil {
		return job, err
	}
	if !deleted || job.State != "pending" {
		return job, ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id FROM session_metadata WHERE json_extract(value,'$.project')=? ORDER BY session_id LIMIT ?`, job.Project.ID, limit)
	if err != nil {
		return job, err
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return job, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return job, err
	}
	now := stamp(time.Now().UTC())
	for _, id := range ids {
		identity, _ := json.Marshal([]string{operation, id})
		hash := sha256.Sum256(identity)
		auditID := "project-delete-batch:" + hex.EncodeToString(hash[:])
		newJSON := `json_set(value,'$.project','','$.projectOverride',json('true'),'$.revision',revision+1,'$.updatedAt',?)`
		_, err = tx.ExecContext(ctx, `INSERT INTO organization_audit SELECT ?,session_id,revision+1,value,`+newJSON+`,json_object('project','','revision',revision,'operationId',?),? FROM session_metadata WHERE session_id=?`, auditID, now, auditID, now, id)
		if err != nil {
			return job, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE session_metadata SET revision=revision+1,value=`+newJSON+` WHERE session_id=?`, now, id); err != nil {
			return job, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('metadata',?,?)`, id, now); err != nil {
			return job, err
		}
	}
	job.Processed += int64(len(ids))
	job.UpdatedAt = now
	var remaining bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_metadata WHERE json_extract(value,'$.project')=?)`, job.Project.ID).Scan(&remaining); err != nil {
		return job, err
	}
	if !remaining {
		job.State = "complete"
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_operations(operation_id,request_hash,result) VALUES(?,?,?)`, operation, requestHash, raw); err != nil {
			return job, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_deletions SET state=?,processed=?,updated_at=?,problem='' WHERE operation_id=?`, job.State, job.Processed, now, operation); err != nil {
		return job, err
	}
	if err = tx.Commit(); err != nil {
		return ProjectDeletion{}, err
	}
	return job, nil
}

// RunProjectDeletions belongs to the hub lifecycle, never a parser child.
// It yields between batches and retains failed work for later retry.
func (s *Store) RunProjectDeletions(ctx context.Context, onError func(error)) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		id, err := s.NextProjectDeletion(attempt)
		if err == nil && id != "" {
			_, err = s.AdvanceProjectDeletion(attempt, id, 100)
		}
		cancel()
		delay := time.Second
		if id != "" {
			delay = 100 * time.Millisecond
		}
		if err != nil && ctx.Err() == nil {
			if onError != nil {
				onError(err)
			}
			delay = 5 * time.Second
			if id != "" {
				problem, stop := context.WithTimeout(ctx, 5*time.Second)
				s.writeMu.Lock()
				_, saveErr := s.db.ExecContext(problem, `UPDATE project_deletions SET problem='Database operation blocked; deletion will retry. See hub diagnostics.',updated_at=? WHERE operation_id=? AND state='pending'`, stamp(time.Now().UTC()), id)
				s.writeMu.Unlock()
				stop()
				if saveErr != nil && onError != nil {
					onError(saveErr)
				}
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
