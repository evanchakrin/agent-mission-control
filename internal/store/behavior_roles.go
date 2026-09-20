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

var roleSuffix = regexp.MustCompile(`\s*#\d+$`)
var roleIdentity = regexp.MustCompile(`[0-9a-f-]{12,}`)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("amc_flow_role", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		name, _ := args[0].(string)
		if name == "" {
			return "Unnamed helper identity", nil
		}
		name = roleIdentity.ReplaceAllString(roleSuffix.ReplaceAllString(name, ""), "·")
		r := []rune(name)
		if len(r) > 30 {
			r = r[:30]
		}
		return string(r), nil
	})
}

type BehaviorRole struct {
	Name                  string   `json:"name"`
	Appearances           int64    `json:"appearances"`
	Sessions              int64    `json:"sessions"`
	Machines              int64    `json:"machines"`
	Errors                int64    `json:"errors"`
	WithErrors            int64    `json:"withErrors"`
	WithoutReportedErrors int64    `json:"withoutReportedErrors"`
	Unknown               int64    `json:"unknown"`
	DurationObserved      int64    `json:"durationObserved"`
	MeanObservedMS        *float64 `json:"meanObservedMs"`
	RecordedTokens        int64    `json:"recordedTokens"`
	PricedTokens          int64    `json:"pricedTokens"`
	UnpricedTokens        int64    `json:"unpricedTokens"`
	UnattributedTokens    int64    `json:"unattributedTokens"`
	KnownCost             *float64 `json:"knownCost"`
	WithEstimate          int64    `json:"withEstimate"`
	ExampleSessionID      string   `json:"exampleSessionId"`
	LastActivity          string   `json:"lastActivity"`
}
type BehaviorRolePage struct {
	Roles            []BehaviorRole `json:"roles"`
	TotalRoles       int64          `json:"totalRoles"`
	TotalAppearances int64          `json:"totalAppearances"`
	Snapshot         string         `json:"snapshot"`
	NextCursor       string         `json:"nextCursor,omitempty"`
}

func (s *Store) BehaviorRoles(ctx context.Context, q SessionQuery) (BehaviorRolePage, error) {
	out := BehaviorRolePage{Roles: []BehaviorRole{}}
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
	if err := s.InitializeAccounting(ctx); err != nil {
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
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(behaviorRolesSQL, where), args...)
	if err != nil {
		return out, err
	}
	hash := sha256.New()
	enc := json.NewEncoder(hash)
	_ = enc.Encode([]string{"behavior-roles-v1", base, where})
	_ = enc.Encode(args)
	more := false
	for rows.Next() {
		var item BehaviorRole
		if err = rows.Scan(&item.Name, &item.Appearances, &item.Sessions, &item.Machines, &item.Errors, &item.WithErrors, &item.WithoutReportedErrors, &item.Unknown, &item.DurationObserved, &item.MeanObservedMS, &item.RecordedTokens, &item.PricedTokens, &item.UnpricedTokens, &item.UnattributedTokens, &item.KnownCost, &item.WithEstimate, &item.ExampleSessionID, &item.LastActivity); err != nil {
			rows.Close()
			return out, err
		}
		out.TotalRoles++
		out.TotalAppearances += item.Appearances
		_ = enc.Encode(item)
		if out.TotalRoles <= cp.Offset {
			continue
		}
		if len(out.Roles) < q.Limit {
			out.Roles = append(out.Roles, item)
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
	if q.Cursor != "" && cp.Snapshot != out.Snapshot {
		return BehaviorRolePage{}, ErrHistoryChanged
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

const behaviorRolesSQL = `WITH selected AS MATERIALIZED (SELECT q.id,q.machine_id,q.last_activity,q.source_id,q.generation,q.projection_revision,q.parent_native_id,q.native_agent_id,q.provider,json_extract(s.projection,'$.pricing.snapshotId') AS pricing_id,COALESCE(json_extract(s.projection,'$.completeness'),'unknown') AS completeness FROM query_sessions q JOIN sessions s ON s.id=q.id WHERE %s),
 ev AS MATERIALIZED (SELECT a.session_id,a.agent_id,a.events,a.errors,a.unknown_results,a.indexing_errors,a.first_at,a.last_at FROM query_event_agents a JOIN selected q ON a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision WHERE a.agent_id<>'' AND (a.agent_id<>'main' OR q.parent_native_id<>'' OR (q.provider='claude' AND q.native_agent_id=a.agent_id))),
 usage AS MATERIALIZED (SELECT u.session_id,u.agent_id,u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out AS tokens,u.unknown_tokens AS unattributed FROM query_agent_usage u JOIN selected q ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision WHERE u.agent_id<>'' AND (u.agent_id<>'main' OR q.parent_native_id<>'' OR (q.provider='claude' AND q.native_agent_id=u.agent_id))),
 ids AS (SELECT session_id,agent_id FROM ev UNION SELECT session_id,agent_id FROM usage),
 prices AS (SELECT q.id,p.agent_id,SUM(json_extract(p.evidence,'$.cost')) AS cost,SUM(json_extract(p.evidence,'$.pricedTokens')) AS priced FROM selected q JOIN accounting_estimates a ON a.session_id=q.id AND a.id=q.pricing_id JOIN observation_prices p ON p.snapshot_id=a.id WHERE json_extract(a.estimate,'$.attributionVersion')=1 GROUP BY q.id,p.agent_id),
 appearances AS (SELECT q.id,q.machine_id,q.last_activity,i.agent_id,amc_flow_role(n.name) AS role,
 COALESCE(e.errors,0) AS errors,CASE WHEN COALESCE(e.errors,0)>0 THEN 'errors' WHEN COALESCE(e.events,0)=0 OR COALESCE(e.unknown_results,0)>0 OR COALESCE(e.indexing_errors,0)>0 OR q.completeness NOT IN ('complete','indexed-source') OR so.source_id IS NULL OR so.indexed_offset<so.durable_offset THEN 'unknown' ELSE 'no-reported-errors' END AS outcome,
 CASE WHEN julianday(e.last_at)>=julianday(e.first_at) THEN (julianday(e.last_at)-julianday(e.first_at))*86400000.0 ELSE NULL END AS observed_ms,
 COALESCE(u.tokens,0) AS tokens,COALESCE(p.priced,0) AS priced,MAX(COALESCE(u.tokens,0)-COALESCE(p.priced,0),0) AS unpriced,COALESCE(u.unattributed,0) AS unattributed,CASE WHEN p.priced>0 THEN p.cost ELSE NULL END AS cost
 FROM ids i JOIN selected q ON q.id=i.session_id LEFT JOIN ev e ON e.session_id=i.session_id AND e.agent_id=i.agent_id LEFT JOIN usage u ON u.session_id=i.session_id AND u.agent_id=i.agent_id LEFT JOIN agent_names n ON n.session_id=i.session_id AND n.agent_id=i.agent_id LEFT JOIN prices p ON p.id=i.session_id AND p.agent_id=i.agent_id LEFT JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation
 WHERE i.agent_id<>'' AND (i.agent_id<>'main' OR q.parent_native_id<>'' OR (q.provider='claude' AND q.native_agent_id=i.agent_id))),
 ranked AS (SELECT *,ROW_NUMBER() OVER(PARTITION BY role ORDER BY last_activity DESC,id COLLATE BINARY,agent_id COLLATE BINARY) AS position FROM appearances)
 SELECT role,COUNT(*),COUNT(DISTINCT id),COUNT(DISTINCT machine_id),SUM(errors),SUM(outcome='errors'),SUM(outcome='no-reported-errors'),SUM(outcome='unknown'),COUNT(observed_ms),AVG(observed_ms),SUM(tokens),SUM(priced),SUM(unpriced),SUM(unattributed),SUM(cost),COUNT(cost),MAX(CASE WHEN position=1 THEN id END),MAX(last_activity)
 FROM ranked GROUP BY role ORDER BY COUNT(*) DESC,role COLLATE BINARY`
