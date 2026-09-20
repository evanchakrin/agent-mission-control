package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"modernc.org/sqlite"
)

var flowToolPrefix = regexp.MustCompile(`^mcp__[^_]+__`)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("amc_flow_tool", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		name, _ := args[0].(string)
		if name == "" || len(name) > 4096 {
			return "(unattributed tool)", nil
		}
		return flowToolPrefix.ReplaceAllString(name, ""), nil
	})
}

type BehaviorPattern struct {
	Fanout           string   `json:"fanout"`
	Tools            []string `json:"tools"`
	Outcome          string   `json:"outcome"`
	Sessions         int64    `json:"sessions"`
	ExampleSessionID string   `json:"exampleSessionId"`
	LastActivity     string   `json:"lastActivity"`
}
type BehaviorPatternPage struct {
	Patterns      []BehaviorPattern `json:"patterns"`
	TotalSessions int64             `json:"totalSessions"`
	TotalPatterns int64             `json:"totalPatterns"`
	Snapshot      string            `json:"snapshot"`
	NextCursor    string            `json:"nextCursor,omitempty"`
}
type behaviorCursor struct {
	Snapshot string
	Offset   int64
}

// BehaviorPatterns groups current indexed observations, not successful runs.
// Every matching session contributes once; only a bounded page is retained in Go.
func (s *Store) BehaviorPatterns(ctx context.Context, q SessionQuery) (BehaviorPatternPage, error) {
	out := BehaviorPatternPage{Patterns: []BehaviorPattern{}}
	if len(q.Cursor) > 8192 || len(q.MachineID) > 1024 || len(q.Provider) > 256 || len(q.Project) > 4096 || q.Limit < 1 || q.Limit > 100 {
		return out, ErrInvalid
	}
	var cp behaviorCursor
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || len(cp.Snapshot) != 64 || cp.Offset < 1 {
			return out, ErrInvalid
		}
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	if err := s.ensureBehaviorTools(ctx); err != nil {
		return out, err
	}
	where, args, err := catalogWhere(q)
	if err != nil {
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
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(behaviorPatternsSQL, where), args...)
	if err != nil {
		return out, err
	}
	hash := sha256.New()
	enc := json.NewEncoder(hash)
	_ = enc.Encode([]string{"behavior-patterns-v1", base, where})
	_ = enc.Encode(args)
	more := false
	for rows.Next() {
		var item BehaviorPattern
		var raw string
		if err = rows.Scan(&item.Fanout, &raw, &item.Outcome, &item.Sessions, &item.ExampleSessionID, &item.LastActivity); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(raw), &item.Tools); err != nil {
			rows.Close()
			return out, err
		}
		out.TotalPatterns++
		out.TotalSessions += item.Sessions
		_ = enc.Encode(item)
		if out.TotalPatterns <= cp.Offset {
			continue
		}
		if len(out.Patterns) < q.Limit {
			out.Patterns = append(out.Patterns, item)
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
	out.Snapshot = hex.EncodeToString(hash.Sum(nil))
	if q.Cursor != "" && out.Snapshot != cp.Snapshot {
		return BehaviorPatternPage{}, ErrHistoryChanged
	}
	if _, err = s.searchSnapshot(ctx, SearchQuery{}, base); err != nil {
		return out, err
	}
	if more {
		raw, _ := json.Marshal(behaviorCursor{out.Snapshot, cp.Offset + int64(q.Limit)})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

const behaviorPatternsSQL = `WITH selected AS MATERIALIZED (SELECT q.id,q.last_activity,q.source_id,q.generation,q.projection_revision,COALESCE(json_extract(s.projection,'$.completeness'),'unknown') AS completeness FROM query_sessions q JOIN sessions s ON s.id=q.id WHERE %s),
 current_agents AS MATERIALIZED (SELECT a.session_id,a.agent_id,a.events,a.errors,a.unknown_results,a.indexing_errors FROM query_event_agents a JOIN selected q ON a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision),
 ` + behaviorTeamSQL + `,
 outcomes AS (SELECT session_id,SUM(events) AS events,SUM(errors) AS errors,SUM(unknown_results)+SUM(indexing_errors) AS unknowns FROM current_agents GROUP BY session_id),
 tools AS (SELECT q.id,e.tool AS name,e.calls FROM selected q JOIN query_flow_tools_v1 e ON e.session_id=q.id AND e.source_id=q.source_id AND e.generation=q.generation AND e.projection_revision=q.projection_revision),
 ranked_tools AS (SELECT *,ROW_NUMBER() OVER(PARTITION BY id ORDER BY calls DESC,name COLLATE BINARY) AS position FROM tools),
 tool_lists AS (SELECT id,json_group_array(name ORDER BY position) AS names FROM ranked_tools WHERE position<=3 GROUP BY id),
 signatures AS (SELECT q.id,q.last_activity,
 CASE WHEN COALESCE(t.unattributed,0)>0 THEN 'incomplete-attribution' WHEN COALESCE(t.agents,0)=0 THEN 'unobserved' WHEN t.agents=1 THEN 'solo' WHEN t.agents<=5 THEN 'small-team' WHEN t.agents<=30 THEN 'team' ELSE 'fleet' END AS fanout,
 COALESCE(l.names,'[]') AS names,
 CASE WHEN COALESCE(o.errors,0)>0 THEN 'reported-errors' WHEN COALESCE(o.events,0)=0 THEN 'no-indexed-events' WHEN COALESCE(o.unknowns,0)>0 OR q.completeness NOT IN ('complete','indexed-source') OR so.source_id IS NULL OR so.indexed_offset<so.durable_offset THEN 'unknown' ELSE 'no-reported-errors' END AS outcome
 FROM selected q LEFT JOIN team t ON t.session_id=q.id LEFT JOIN outcomes o ON o.session_id=q.id LEFT JOIN tool_lists l ON l.id=q.id LEFT JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation),
 grouped AS (SELECT fanout,names,outcome,COUNT(*) AS sessions,MAX(last_activity) AS last_activity FROM signatures GROUP BY fanout,names,outcome)
 SELECT g.fanout,g.names,g.outcome,g.sessions,MIN(s.id COLLATE BINARY),g.last_activity
 FROM grouped g JOIN signatures s ON s.fanout=g.fanout AND s.names=g.names AND s.outcome=g.outcome AND s.last_activity=g.last_activity
 GROUP BY g.fanout,g.names,g.outcome,g.sessions,g.last_activity
 ORDER BY g.sessions DESC,g.fanout COLLATE BINARY,g.names COLLATE BINARY,g.outcome COLLATE BINARY`

// Reuse exact distinct-agent seeks; unknown identities remain a separate flag.
// Unlike counting every usage row, this work scales with recorded identities.
const behaviorTeamSQL = `team AS MATERIALIZED (SELECT q.id AS session_id,` + calendarAgentScopeCount + ` AS agents,
 (EXISTS(SELECT 1 FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision AND a.agent_id='')
 OR EXISTS(SELECT 1 FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.agent_id='')) AS unattributed FROM selected q)`
