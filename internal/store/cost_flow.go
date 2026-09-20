package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type CostFlowPage struct {
	Session Session `json:"session"`
	AgentPage
	Snapshot  string            `json:"snapshot"`
	TopCost   []FlowContributor `json:"topCost"`
	TopOutput []FlowContributor `json:"topOutput"`
}

type FlowContributor struct {
	ID    string  `json:"id"`
	Name  string  `json:"name,omitempty"`
	Value float64 `json:"value"`
}

const topFlowOutputSQL = `SELECT u.agent_id,COALESCE((SELECT name FROM agent_names n WHERE n.session_id=q.id AND n.agent_id=u.agent_id),''),SUM(u.tokens_out) AS weight
 FROM query_sessions q JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
 WHERE q.id=? GROUP BY u.agent_id HAVING weight>0 ORDER BY weight DESC,u.agent_id LIMIT 14`

const topFlowCostSQL = `SELECT p.agent_id,COALESCE((SELECT name FROM agent_names n WHERE n.session_id=s.id AND n.agent_id=p.agent_id),''),SUM(json_extract(p.evidence,'$.cost')) AS weight
 FROM sessions s JOIN accounting_estimates a ON a.id=json_extract(s.projection,'$.pricing.snapshotId') AND a.session_id=s.id
 JOIN observation_prices p ON p.snapshot_id=a.id
 WHERE s.id=? AND json_extract(a.estimate,'$.attributionVersion')=1
 GROUP BY p.agent_id HAVING weight>0 AND SUM(json_extract(p.evidence,'$.pricedTokens'))>0 ORDER BY weight DESC,p.agent_id LIMIT 14`

func (s *Store) topFlow(ctx context.Context, id string, cost bool) ([]FlowContributor, error) {
	query := topFlowOutputSQL
	if cost {
		if err := s.InitializeAccounting(ctx); err != nil {
			return nil, err
		}
		query = topFlowCostSQL
	}
	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowContributor{}
	for rows.Next() {
		var item FlowContributor
		if err = rows.Scan(&item.ID, &item.Name, &item.Value); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Flow totals and attribution must refer to the same published accounting and
// indexed projection. Unlike event cursors, these aggregate cursors also expire
// on append or repricing. Organization edits do not change recorded accounting.
func (s *Store) costFlowSnapshot(row Session) string {
	row.Metadata = Metadata{}
	b, _ := json.Marshal(struct {
		Epoch   string
		Session Session
	}{s.epoch, row})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) costFlowCheckpoint(ctx context.Context, row Session) (string, error) {
	state, err := s.SourceState(ctx, row.SourceID, row.Generation)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	// Cache-only imported projections can lack source state. Keep that absence
	// distinct from a captured source at offset zero; later recovery expires it.
	b, _ := json.Marshal([]any{s.costFlowSnapshot(row), err == nil, state.IndexedOffset, state.ProjectionRevision})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *Store) SessionCostFlow(ctx context.Context, id, cursor string, limit int, snapshot string) (CostFlowPage, error) {
	if limit < 1 || limit > 100 || cursor != "" && snapshot == "" {
		return CostFlowPage{}, ErrInvalid
	}
	row, err := s.GetSession(ctx, id)
	if err != nil {
		return CostFlowPage{}, err
	}
	token, err := s.costFlowCheckpoint(ctx, row)
	if err != nil {
		return CostFlowPage{}, err
	}
	if snapshot != "" && snapshot != token {
		return CostFlowPage{}, ErrHistoryChanged
	}
	agents, err := s.SessionAgents(ctx, id, cursor, limit)
	if err != nil {
		return CostFlowPage{}, err
	}
	topCost, err := s.topFlow(ctx, id, true)
	if err != nil {
		return CostFlowPage{}, err
	}
	topOutput, err := s.topFlow(ctx, id, false)
	if err != nil {
		return CostFlowPage{}, err
	}
	after, err := s.GetSession(ctx, id)
	if err != nil {
		return CostFlowPage{}, err
	}
	afterToken, err := s.costFlowCheckpoint(ctx, after)
	if err != nil {
		return CostFlowPage{}, err
	}
	if afterToken != token {
		return CostFlowPage{}, ErrHistoryChanged
	}
	return CostFlowPage{Session: row, AgentPage: agents, Snapshot: token, TopCost: topCost, TopOutput: topOutput}, nil
}
