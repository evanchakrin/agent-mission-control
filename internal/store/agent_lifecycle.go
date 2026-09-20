package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type AgentLifecycle struct {
	AgentID        string    `json:"agentId"`
	Snapshot       string    `json:"snapshot"`
	ObservedAt     time.Time `json:"observedAt"`
	Retrying       *bool     `json:"retrying"`
	Pending        *Event    `json:"pending,omitempty"`
	Stalled        *bool     `json:"stalled"`
	UncertainCalls int64     `json:"uncertainCalls"`
}

// Lifecycle evidence uses the complete current interpretation of one agent.
// SQLite groups call identities; no corpus-sized pending-call map is retained
// in Go. Unknown result status and ambiguous IDs never become a clean result.
func (s *Store) AgentLifecycle(ctx context.Context, id, agent, snapshot string, now time.Time) (AgentLifecycle, error) {
	out := AgentLifecycle{AgentID: agent, ObservedAt: now.UTC()}
	if agent == "" || len(agent) > 4096 || now.IsZero() {
		return out, ErrInvalid
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return out, err
	}
	if err = s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	const scope = ` FROM events e JOIN query_sessions q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision WHERE q.id=? AND e.agent_id=?`
	var pending int64
	err = tx.QueryRowContext(ctx, `WITH evidence AS (SELECT e.seq,e.kind,e.data`+scope+` AND e.kind IN ('tool-call','tool-result')),
 pairs AS (SELECT json_extract(data,'$.toolUseId') AS key,
 SUM(kind='tool-call') AS calls,SUM(kind='tool-result') AS results,
 MAX(CASE WHEN kind='tool-call' THEN seq ELSE 0 END) AS call_seq,
 MAX(CASE WHEN kind='tool-result' THEN seq ELSE 0 END) AS result_seq
 FROM evidence WHERE json_type(data,'$.toolUseId')='text' AND json_extract(data,'$.toolUseId')<>'' GROUP BY key)
 SELECT COALESCE(MAX(CASE WHEN calls=1 AND results=0 THEN call_seq ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN calls<>1 OR results>1 OR results=1 AND result_seq<=call_seq THEN 1 ELSE 0 END),0)+
 (SELECT COUNT(*) FROM evidence WHERE COALESCE(json_type(data,'$.toolUseId'),'')<>'text' OR json_extract(data,'$.toolUseId')='') FROM pairs`, id, agent).Scan(&pending, &out.UncertainCalls)
	if err != nil {
		return out, err
	}
	var kind string
	var data []byte
	var lastSeq int64
	err = tx.QueryRowContext(ctx, `SELECT e.kind,e.data,e.seq`+scope+` AND e.kind IN ('tool-call','tool-result') ORDER BY e.seq DESC LIMIT 1`, id, agent).Scan(&kind, &data, &lastSeq)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	no := false
	if err == sql.ErrNoRows || kind == "tool-call" {
		out.Retrying = &no
	} else {
		var result struct {
			Error *bool  `json:"error"`
			Key   string `json:"toolUseId"`
		}
		if json.Unmarshal(data, &result) == nil && result.Error != nil {
			if !*result.Error {
				out.Retrying = &no
			} else if result.Key != "" {
				var calls, results int
				var seq int64
				if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(e.kind='tool-call'),0),COALESCE(SUM(e.kind='tool-result'),0),COALESCE(MAX(CASE WHEN e.kind='tool-call' THEN e.seq ELSE 0 END),0)`+scope+` AND e.kind IN ('tool-call','tool-result') AND json_extract(e.data,'$.toolUseId')=?`, id, agent, result.Key).Scan(&calls, &results, &seq); err != nil {
					return out, err
				}
				if calls == 1 && results == 1 && seq < lastSeq {
					yes := true
					out.Retrying = &yes
				}
			}
		}
	}
	if out.UncertainCalls == 0 {
		out.Stalled = &no
	}
	if pending != 0 {
		var e Event
		var timestamp string
		err = tx.QueryRowContext(ctx, `SELECT e.id,e.seq,e.agent_id,e.kind,e.timestamp,e.text,e.data,e.source_offset,e.raw_length,e.projection_revision`+scope+` AND e.seq=?`, id, agent, pending).Scan(&e.ID, &e.Sequence, &e.AgentID, &e.Kind, &timestamp, &e.Text, &e.Data, &e.SourceOffset, &e.SourceLength, &e.ProjectionRevision)
		if err != nil {
			return out, err
		}
		e.SessionID = id
		e.HistorySnapshot = token
		e.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || e.Timestamp.IsZero() || e.Timestamp.After(now) {
			out.Stalled = nil
		} else {
			var raw []byte
			if err = tx.QueryRowContext(ctx, `SELECT so.meta_json FROM query_sessions q JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation WHERE q.id=?`, id).Scan(&raw); err != nil {
				return out, err
			}
			var src protocol.Source
			if err = json.Unmarshal(raw, &src); err != nil {
				return out, err
			}
			if src.ModifiedAt.IsZero() || src.ModifiedAt.After(now) {
				out.Stalled = nil
			} else {
				stalled := now.Sub(e.Timestamp) > 2*time.Minute && now.Sub(src.ModifiedAt) < 10*time.Minute
				if stalled || out.UncertainCalls == 0 {
					out.Stalled = &stalled
				}
			}
		}
		out.Pending = &e
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return AgentLifecycle{}, err
	}
	out.Snapshot = token
	return out, nil
}
