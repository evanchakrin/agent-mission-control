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

func (s *Store) GetMetadata(ctx context.Context, id string) (Metadata, error) {
	var m Metadata
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM session_metadata WHERE session_id=?`, id).Scan(&b)
	if err == sql.ErrNoRows {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func (s *Store) PatchMetadata(ctx context.Context, id string, p MetadataPatch) (Metadata, error) {
	return s.PatchMetadataWithStage(ctx, id, p, nil)
}

// PatchMetadataWithStage exposes fixed diagnostic stage names, never session
// IDs, organization values, or SQL parameters. Mutation semantics are unchanged.
func (s *Store) PatchMetadataWithStage(ctx context.Context, id string, p MetadataPatch, stage func(string)) (Metadata, error) {
	if stage == nil {
		stage = func(string) {}
	}
	stage("organization-validate")
	var empty Metadata
	if !validID(id) || !validID(p.OperationID) || p.Revision < 0 {
		return empty, ErrInvalid
	}
	if (p.Name != nil && len(*p.Name) > 1024) || (p.Note != nil && len(*p.Note) > 65536) || (p.Tags != nil && len(*p.Tags) > 1000) || (p.Project != nil && len(*p.Project) > 1024) {
		return empty, ErrInvalid
	}
	if p.AgentName != nil && !validAgentName(*p.AgentName) {
		return empty, ErrInvalid
	}
	if p.Tags != nil {
		for _, tag := range *p.Tags {
			if len(tag) > 1024 {
				return empty, ErrInvalid
			}
		}
	}
	request, err := json.Marshal(p)
	if err != nil {
		return empty, err
	}
	h := sha256.Sum256(request)
	requestHash := hex.EncodeToString(h[:])
	stage("organization-writer-admission")
	if err := s.writeMu.LockPriorityContext(ctx); err != nil {
		return empty, err
	}
	defer s.writeMu.Unlock()
	stage("organization-begin-transaction")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	stage("organization-load-metadata")
	var priorSession, priorHash string
	var result []byte
	err = tx.QueryRowContext(ctx, `SELECT session_id,request_hash,result FROM metadata_operations WHERE operation_id=?`, p.OperationID).Scan(&priorSession, &priorHash, &result)
	if err == nil {
		if priorSession != id || priorHash != requestHash {
			return empty, fmt.Errorf("%w: operation ID reused", ErrConflict)
		}
		var m Metadata
		err = json.Unmarshal(result, &m)
		return m, err
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	var m Metadata
	var b []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM session_metadata WHERE session_id=?`, id).Scan(&b)
	if err == nil {
		if err = json.Unmarshal(b, &m); err != nil {
			return empty, err
		}
	} else if err != sql.ErrNoRows {
		return empty, err
	}
	if err == sql.ErrNoRows {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id=?`, id).Scan(&exists); err == sql.ErrNoRows {
			return empty, ErrNotFound
		} else if err != nil {
			return empty, err
		}
	}
	if m.Revision != p.Revision {
		return m, fmt.Errorf("%w: expected metadata revision %d", ErrConflict, m.Revision)
	}
	before, err := json.Marshal(m)
	if err != nil {
		return empty, err
	}
	if p.AgentName != nil {
		var priorName string
		err = tx.QueryRowContext(ctx, `SELECT name FROM agent_names WHERE session_id=? AND agent_id=?`, id, p.AgentName.ID).Scan(&priorName)
		if err != nil && err != sql.ErrNoRows {
			return empty, err
		}
		before, err = json.Marshal(struct {
			Metadata
			AgentName AgentNamePatch `json:"agentName"`
		}{m, AgentNamePatch{ID: p.AgentName.ID, Name: priorName}})
		if err != nil {
			return empty, err
		}
		// Empty names are retained as tombstones, so a later import cannot
		// resurrect a name explicitly cleared by the owner.
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_names VALUES(?,?,?) ON CONFLICT(session_id,agent_id) DO UPDATE SET name=excluded.name`, id, p.AgentName.ID, p.AgentName.Name); err != nil {
			return empty, err
		}
	}
	if p.Archived != nil {
		m.Archived = *p.Archived
	}
	if p.Pinned != nil {
		m.Pinned = *p.Pinned
	}
	if p.Name != nil {
		m.Name = *p.Name
	}
	if p.Note != nil {
		m.Note = *p.Note
	}
	if p.Project != nil {
		var deleted bool
		err = tx.QueryRowContext(ctx, "SELECT deleted FROM projects WHERE id=?", *p.Project).Scan(&deleted)
		if err != nil && err != sql.ErrNoRows {
			return empty, err
		}
		if deleted {
			return empty, fmt.Errorf("%w: project was deleted", ErrConflict)
		}
		m.Project = *p.Project
		m.ProjectOverride = true
	}
	if p.Tags != nil {
		m.Tags = append([]string{}, (*p.Tags)...)
	}
	m.Revision++
	m.UpdatedAt = time.Now().UTC()
	b, err = json.Marshal(m)
	if err != nil {
		return empty, err
	}
	stage("organization-update-catalog")
	_, err = tx.ExecContext(ctx, `INSERT INTO session_metadata VALUES(?,?,?) ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,value=excluded.value`, id, m.Revision, b)
	if err != nil {
		return empty, err
	}
	stage("organization-write-journal")
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata_operations VALUES(?,?,?,?)`, p.OperationID, id, requestHash, b); err != nil {
		return empty, err
	}
	after := b
	if p.AgentName != nil {
		after, err = json.Marshal(struct {
			Metadata
			AgentName *AgentNamePatch `json:"agentName"`
		}{m, p.AgentName})
		if err != nil {
			return empty, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO organization_audit VALUES(?,?,?,?,?,?,?)`, p.OperationID, id, m.Revision, before, after, request, stamp(m.UpdatedAt)); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('metadata',?,?)`, id, stamp(m.UpdatedAt)); err != nil {
		return empty, err
	}
	stage("organization-commit")
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return m, nil
}

func validAgentName(p AgentNamePatch) bool {
	return p.ID != "" && len(p.ID) <= 4096 && !strings.ContainsRune(p.ID, 0) && len(p.Name) <= 1024
}

// ImportLegacyMetadata is repeatable and never replaces a metadata row already
// owned by the user. Unmapped keys keep their original identity until a later
// explicit alias migration, so even missing-source archive flags are preserved.
func (s *Store) ImportLegacyMetadata(ctx context.Context, aliases map[string]string, entries map[string]json.RawMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS legacy_metadata(legacy_key TEXT PRIMARY KEY,original_json BLOB NOT NULL)`); err != nil {
		return err
	}
	for key, raw := range entries {
		id := aliases[key]
		if id == "" {
			id = key
		}
		if !validID(key) || !validID(id) || !json.Valid(raw) {
			return fmt.Errorf("%w: legacy metadata", ErrInvalid)
		}
		var prior string
		err = tx.QueryRowContext(ctx, `SELECT session_id FROM legacy_aliases WHERE legacy_key=?`, key).Scan(&prior)
		if err == nil && aliases[key] == "" {
			// A late transcript may already have resolved an imported placeholder.
			// Replaying the original import must not revert that durable identity.
			id = prior
		}
		if err == nil && prior != id {
			return fmt.Errorf("%w: legacy alias %s already points elsewhere", ErrConflict, key)
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		var old struct {
			AgentNames map[string]string `json:"agentNames"`
			Archived   bool              `json:"archived"`
			Pinned     bool              `json:"pinned"`
			Name       string            `json:"name"`
			Note       string            `json:"note"`
			Tags       []string          `json:"tags"`
			Project    *string           `json:"project"`
			ProjectID  json.RawMessage   `json:"projectId"`
		}
		if err = json.Unmarshal(raw, &old); err != nil {
			return err
		}
		if old.Project == nil && len(old.ProjectID) > 0 {
			var legacyID string
			if err = json.Unmarshal(old.ProjectID, &legacyID); err != nil {
				return err
			}
			old.Project = &legacyID // A legacy null means explicit unassignment.
		}
		for agent, name := range old.AgentNames {
			if !validAgentName(AgentNamePatch{ID: agent, Name: name}) {
				return fmt.Errorf("%w: legacy agent name", ErrInvalid)
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_names VALUES(?,?,?)`, id, agent, name); err != nil {
				return err
			}
		}
		m := Metadata{Archived: old.Archived, Pinned: old.Pinned, Name: old.Name, Note: old.Note, Tags: old.Tags, Revision: 1, UpdatedAt: time.Now().UTC()}
		if old.Project != nil {
			m.Project, m.ProjectOverride = *old.Project, true
			var deleted bool
			err = tx.QueryRowContext(ctx, "SELECT deleted FROM projects WHERE id=?", m.Project).Scan(&deleted)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if deleted {
				// Preserve original import evidence below, but honor the owner's
				// project tombstone when creating new organization rows.
				m.Project = ""
			}
		}
		b, _ := json.Marshal(m)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_aliases VALUES(?,?)`, key, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_metadata VALUES(?,?)`, key, []byte(raw)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_metadata VALUES(?,?,?)`, id, m.Revision, b)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('metadata',?,?)`, id, stamp(m.UpdatedAt)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) ResolveAlias(ctx context.Context, key string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM legacy_aliases WHERE legacy_key=?`, key).Scan(&id)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return id, err
}
