package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Only call/result evidence is indexed. Generation and parser revision are part
// of the identity: a reused provider call ID cannot join old interpretations.
func (s *Store) ensureToolMatchIndex(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS events_tool_match ON events(session_id,source_id,generation,projection_revision,agent_id,json_extract(data,'$.toolUseId'),kind,seq) WHERE kind IN ('tool-call','tool-result');
 CREATE INDEX IF NOT EXISTS events_tool_call_page ON events(session_id,source_id,generation,projection_revision,seq) WHERE kind='tool-call'`)
	return err
}

type ToolSpan struct {
	Call           Event      `json:"call"`
	State          string     `json:"state"`
	ResultSequence *int64     `json:"resultSequence,omitempty"`
	EndedAt        *time.Time `json:"endedAt,omitempty"`
	DurationMillis *int64     `json:"durationMillis,omitempty"`
	Error          *bool      `json:"error,omitempty"`
}

type ToolSpanPage struct {
	Spans        []ToolSpan `json:"spans"`
	NextSequence int64      `json:"nextSequence,omitempty"`
	Snapshot     string     `json:"snapshot"`
}

const toolCallWhere = `e.session_id=? AND e.kind='tool-call' AND e.seq>?`

const toolMatchSQL = `SELECT e.seq,e.timestamp,CASE WHEN json_type(e.data,'$.error') IN ('true','false') THEN json_extract(e.data,'$.error') END FROM events e INDEXED BY events_tool_match
 JOIN query_sessions q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision
 WHERE e.kind IN ('tool-call','tool-result') AND e.session_id=? AND e.agent_id=? AND json_extract(e.data,'$.toolUseId')=? AND e.kind=? ORDER BY e.seq LIMIT 2`

// ToolSpans matches beyond the current page, but never guesses between repeated
// call IDs. Each identity lookup returns at most two rows to detect ambiguity.
func (s *Store) ToolSpans(ctx context.Context, id string, after int64, limit int, snapshot string) (ToolSpanPage, error) {
	result := ToolSpanPage{Spans: []ToolSpan{}}
	if after < 0 || limit < 1 || limit > 100 || (after > 0 && snapshot == "") {
		return result, ErrInvalid
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return result, err
	}
	page, err := s.queryEvents(ctx, toolCallWhere, []any{id, after}, limit, false)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, call := range page.Events {
		span := ToolSpan{Call: call, State: "missing-id"}
		if call.AgentID == "" {
			span.State = "missing-agent"
			result.Spans = append(result.Spans, span)
			continue
		}
		var data struct {
			ToolUseID string `json:"toolUseId"`
		}
		if json.Unmarshal(call.Data, &data) != nil || data.ToolUseID == "" {
			result.Spans = append(result.Spans, span)
			continue
		}
		counts := [2]int{}
		var resultSeq int64
		var resultTime string
		var resultError sql.NullBool
		for i, kind := range []string{"tool-call", "tool-result"} {
			rows, readErr := tx.QueryContext(ctx, toolMatchSQL, id, call.AgentID, data.ToolUseID, kind)
			if readErr != nil {
				return result, readErr
			}
			for rows.Next() {
				var seq int64
				var timestamp string
				var failed sql.NullBool
				if readErr = rows.Scan(&seq, &timestamp, &failed); readErr != nil {
					rows.Close()
					return result, readErr
				}
				counts[i]++
				if i == 1 {
					resultSeq, resultTime, resultError = seq, timestamp, failed
				}
			}
			readErr = rows.Err()
			rows.Close()
			if readErr != nil {
				return result, readErr
			}
		}
		span.State = "missing-result"
		if counts[0] != 1 || counts[1] > 1 {
			span.State = "ambiguous"
		} else if counts[1] == 1 {
			end, parseErr := time.Parse(time.RFC3339Nano, resultTime)
			span.State = "invalid-time-or-order"
			if parseErr == nil && !call.Timestamp.IsZero() && !end.IsZero() && !end.Before(call.Timestamp) && resultSeq > call.Sequence {
				span.State = "matched"
				span.ResultSequence = &resultSeq
				span.EndedAt = &end
				elapsed := end.UnixMilli() - call.Timestamp.UnixMilli()
				span.DurationMillis = &elapsed
				if resultError.Valid {
					v := resultError.Bool
					span.Error = &v
				}
			}
		}
		result.Spans = append(result.Spans, span)
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return ToolSpanPage{}, err
	}
	result.NextSequence = page.NextSequence
	result.Snapshot = token
	return result, nil
}
