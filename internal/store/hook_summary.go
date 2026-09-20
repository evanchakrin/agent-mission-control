package store

import (
	"context"
	"database/sql"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
)

type JavaScriptHookSummary struct {
	Scope        string                       `json:"scope"`
	Sessions     int64                        `json:"sessions"`
	NeedsRebuild int64                        `json:"needsRebuild"`
	Incomplete   int64                        `json:"incomplete"`
	Evidence     hookideas.JavaScriptEvidence `json:"evidence"`
	Proposed     bool                         `json:"proposed"`
}

const hookSummaryRows = `WITH evidence AS (
 SELECT s.id,h.edited,h.errors,
 CASE WHEN h.source_id IS NULL OR h.indexed_offset<>r.indexed_offset THEN 'missing'
 WHEN h.diagnostics>0 OR r.durable_offset<>r.indexed_offset
 OR COALESCE(json_extract(s.projection,'$.completeness'),'') NOT IN ('indexed-source','complete') THEN 'incomplete'
 ELSE 'ready' END AS state
 FROM sessions s JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation
 LEFT JOIN hook_javascript_evidence h ON h.source_id=s.source_id AND h.generation=s.generation
 AND h.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'')
) `

// JavaScriptHookSummary covers all current session projections, including archived
// sessions. Only evidence covering their durable indexed prefix supports proposals.
// Two bounded-output reads share a short snapshot; no raw/event corpus is scanned.
func (s *Store) JavaScriptHookSummary(ctx context.Context) (JavaScriptHookSummary, error) {
	out := JavaScriptHookSummary{Scope: "all-current-sessions-including-archived"}
	out.Evidence.Examples = []hookideas.JavaScriptExample{}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, hookSummaryRows+`SELECT COUNT(*),
 COALESCE(SUM(state='missing'),0),COALESCE(SUM(state='incomplete'),0),
 COALESCE(SUM(state='ready'),0),COALESCE(SUM(state='ready' AND edited=1),0),
 COALESCE(SUM(state='ready' AND edited=1 AND errors>0),0),
 COALESCE(SUM(CASE WHEN state='ready' THEN errors ELSE 0 END),0) FROM evidence`).Scan(&out.Sessions, &out.NeedsRebuild, &out.Incomplete, &out.Evidence.SessionsRead, &out.Evidence.SessionsEdited, &out.Evidence.SessionsWithErrors, &out.Evidence.Errors)
	if err != nil {
		return JavaScriptHookSummary{}, err
	}
	rows, err := tx.QueryContext(ctx, hookSummaryRows+`SELECT id,errors FROM evidence WHERE state='ready' AND edited=1 AND errors>0 ORDER BY errors DESC,id LIMIT ?`, hookideas.MaxExamples)
	if err != nil {
		return JavaScriptHookSummary{}, err
	}
	for rows.Next() {
		var example hookideas.JavaScriptExample
		if err := rows.Scan(&example.SessionID, &example.Errors); err != nil {
			rows.Close()
			return JavaScriptHookSummary{}, err
		}
		out.Evidence.Examples = append(out.Evidence.Examples, example)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return JavaScriptHookSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return JavaScriptHookSummary{}, err
	}
	out.Proposed = out.Evidence.Proposed()
	return out, nil
}
