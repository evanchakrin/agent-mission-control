package store

import (
	"context"
	"encoding/json"
	"time"
)

type OrganizationAudit struct {
	OperationID string               `json:"operationId"`
	SessionID   string               `json:"sessionId"`
	Revision    int64                `json:"revision"`
	Before      OrganizationSnapshot `json:"before"`
	After       OrganizationSnapshot `json:"after"`
	Patch       MetadataPatch        `json:"patch"`
	At          time.Time            `json:"at"`
}

type OrganizationSnapshot struct {
	Metadata
	AgentName *AgentNamePatch `json:"agentName,omitempty"`
}
type OrganizationAuditPage struct {
	Entries []OrganizationAudit `json:"entries"`
	Next    int64               `json:"next,omitempty"`
}

func (s *Store) OrganizationHistory(ctx context.Context, id string, after int64, limit int) (OrganizationAuditPage, error) {
	p := OrganizationAuditPage{Entries: []OrganizationAudit{}}
	if !validID(id) || after < 0 {
		return p, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,session_id,revision,before_json,after_json,patch_json,at FROM organization_audit WHERE session_id=? AND revision>? ORDER BY revision LIMIT ?`, id, after, pageLimit(limit))
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry OrganizationAudit
		var before, current, patch []byte
		var at string
		if err = rows.Scan(&entry.OperationID, &entry.SessionID, &entry.Revision, &before, &current, &patch, &at); err != nil {
			return p, err
		}
		for _, v := range []struct {
			b      []byte
			target any
		}{{before, &entry.Before}, {current, &entry.After}, {patch, &entry.Patch}} {
			if err = json.Unmarshal(v.b, v.target); err != nil {
				return p, err
			}
		}
		entry.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return p, err
		}
		p.Entries = append(p.Entries, entry)
	}
	if len(p.Entries) == pageLimit(limit) {
		p.Next = p.Entries[len(p.Entries)-1].Revision
	}
	return p, rows.Err()
}
