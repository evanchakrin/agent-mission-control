package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"time"
)

type RunActivity struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	MachineID    string    `json:"machineId"`
	Revision     string    `json:"revision"`
	LastActivity time.Time `json:"lastActivity"`
	Active       bool      `json:"active"`
	Synced       bool      `json:"synced"`
	Stalled      *bool     `json:"stalled"`
}
type RunActivityPage struct {
	Runs          []RunActivity `json:"runs"`
	RecoveryEpoch string        `json:"recoveryEpoch"`
	Truncated     bool          `json:"truncated"`
}

// Recent activity only: no full-history scan or corpus-sized in-memory cache.
// The UI treats ten minutes of inactivity as inferred completion, not success.
func (s *Store) RecentRunActivity(ctx context.Context, now time.Time) (RunActivityPage, error) {
	out := RunActivityPage{Runs: []RunActivity{}, RecoveryEpoch: s.RecoveryEpoch()}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT q.id,q.title,q.machine_id,q.generation,q.projection_revision,q.last_activity,r.meta_json,r.durable_offset,r.indexed_offset
 FROM query_sessions q JOIN sources r ON r.source_id=q.source_id AND r.generation=q.generation
 WHERE q.last_activity>=? AND q.last_activity<=? ORDER BY q.last_activity DESC,q.id LIMIT 101`, stamp(now.Add(-20*time.Minute)), stamp(now))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r RunActivity
		var generation, revision, at string
		var raw []byte
		var durable, indexed int64
		if err = rows.Scan(&r.ID, &r.Title, &r.MachineID, &generation, &revision, &at, &raw, &durable, &indexed); err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Runs) == 100 {
			out.Truncated = true
			break
		}
		var src protocol.Source
		if err = json.Unmarshal(raw, &src); err != nil {
			rows.Close()
			return out, err
		}
		r.LastActivity, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return out, err
		}
		r.Revision = generation + ":" + revision
		r.Active = now.Sub(r.LastActivity) < 10*time.Minute
		r.Synced = durable == indexed && indexed == src.Size && !src.ModifiedAt.After(now)
		out.Runs = append(out.Runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for i := range out.Runs {
		r := &out.Runs[i]
		if !r.Synced {
			continue
		}
		events, e := tx.QueryContext(ctx, `SELECT e.agent_id,e.kind,e.timestamp,CASE WHEN e.kind IN ('tool-call','tool-result') THEN json_extract(e.data,'$.toolUseId') ELSE NULL END FROM events e JOIN query_sessions q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision
 WHERE q.id=? AND e.timestamp>=? AND e.timestamp<=? ORDER BY e.timestamp DESC,e.seq DESC LIMIT 501`, r.ID, stamp(now.Add(-10*time.Minute)), stamp(now))
		if e != nil {
			return out, e
		}
		type call struct {
			calls, results int
			at             time.Time
		}
		pairs := map[string]*call{}
		count := 0
		known := true
		for events.Next() {
			var agent, kind, at string
			var toolID sql.NullString
			if err = events.Scan(&agent, &kind, &at, &toolID); err != nil {
				events.Close()
				return out, err
			}
			count++
			if count > 500 {
				known = false
				break
			}
			if kind != "tool-call" && kind != "tool-result" {
				continue
			}
			if !toolID.Valid || toolID.String == "" {
				known = false
				continue
			}
			key := agent + "\x00" + toolID.String
			p := pairs[key]
			if p == nil {
				p = &call{}
				pairs[key] = p
			}
			if kind == "tool-call" {
				p.calls++
				p.at, err = time.Parse(time.RFC3339Nano, at)
				if err != nil {
					known = false
				}
			} else {
				p.results++
			}
		}
		err = events.Err()
		events.Close()
		if err != nil {
			return out, err
		}
		stalled := false
		for _, p := range pairs {
			if p.calls > 1 || p.results > 1 {
				known = false
			}
			if p.calls == 1 && p.results == 0 && now.Sub(p.at) > 2*time.Minute && r.Active {
				stalled = true
			}
		}
		if known {
			r.Stalled = &stalled
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}
