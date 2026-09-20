package store

import "context"

// ModelHookSummary describes recorded usage attribution, not spawn parameters.
// Agents are session/agent pairs, not unique processes across the fleet. Empty
// agent IDs remain unattributed and never become invented agent identities.
type ModelHookSummary struct {
	Scope                string `json:"scope"`
	Agents               int64  `json:"agents"`
	WithRecordedModel    int64  `json:"withRecordedModel"`
	WithoutRecordedModel int64  `json:"withoutRecordedModel"`
	UnattributedContexts int64  `json:"unattributedContexts"`
}

// ModelHookSummary reads the current published catalog in one SQL snapshot.
// It returns fixed-size output without loading event bodies or usage rows into Go.
// A named observation with zero tokens is still model evidence. A named model on
// an incomplete-attribution observation is not evidence for that specific agent.
func (s *Store) ModelHookSummary(ctx context.Context) (ModelHookSummary, error) {
	out := ModelHookSummary{Scope: "all-current-session-agent-pairs-including-archived"}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	err := s.db.QueryRowContext(ctx, `WITH evidence AS (
 SELECT a.session_id,a.agent_id,0 AS named
 FROM query_event_agents a JOIN query_sessions q
 ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation
 AND q.projection_revision=a.projection_revision
 UNION ALL
 SELECT a.session_id,a.agent_id,CASE WHEN trim(a.model)<>'' AND a.kind<>'incomplete-attribution' THEN 1 ELSE 0 END
 FROM query_usage a JOIN query_sessions q
 ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation
 AND q.projection_revision=a.projection_revision
), agents AS (
 SELECT session_id,agent_id,MAX(named) AS named FROM evidence GROUP BY session_id,agent_id
)
 SELECT COALESCE(SUM(agent_id<>''),0),
 COALESCE(SUM(agent_id<>'' AND named=1),0),
 COALESCE(SUM(agent_id<>'' AND named=0),0),
 COALESCE(SUM(agent_id=''),0) FROM agents`).Scan(
		&out.Agents, &out.WithRecordedModel, &out.WithoutRecordedModel, &out.UnattributedContexts)
	return out, err
}
