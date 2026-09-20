package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"modernc.org/sqlite"
)

func init() {
	// Match the dashboard's case-insensitive text search beyond SQLite's
	// ASCII-only LIKE folding. Inputs remain bounded by metadata/query limits.
	sqlite.MustRegisterDeterministicScalarFunction("amc_fold", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		text, _ := args[0].(string)
		return strings.ToLower(text), nil
	})
	// JSON time.Time values omit trailing fractional zeroes and may carry offsets.
	// Normalize once at read-model write time, preserving nanosecond ordering and
	// UTC day boundaries without reading the corpus into application memory.
	sqlite.MustRegisterDeterministicScalarFunction("amc_timestamp", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		raw, ok := args[0].(string)
		if !ok || raw == "" {
			return "", nil
		}
		value, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || value.IsZero() {
			return "", nil
		}
		return stamp(value), nil
	})
}

// AggregateTotals describes observed history. A missing estimate is not zero
// price, and the presence of one session estimate never proves full coverage.
type AggregateTotals struct {
	PricingTotals
	Sessions                int64             `json:"sessions"`
	Events                  int64             `json:"events"`
	TokensIn                int64             `json:"tokensIn"`
	TokensCache             int64             `json:"tokensCache"`
	TokensCacheWrite        int64             `json:"tokensCacheWrite"`
	TokensOut               int64             `json:"tokensOut"`
	CostEstimate            *float64          `json:"costEstimate"`
	SessionsWithEstimate    int64             `json:"sessionsWithEstimate"`
	SessionsWithoutEstimate int64             `json:"sessionsWithoutEstimate"`
	PricingCoverage         string            `json:"pricingCoverage"`
	CurrentComparisons      *ComparisonTotals `json:"currentComparisons,omitempty"`
}

type SessionStatistics struct {
	SessionID                    string     `json:"sessionId"`
	AgentCount                   int64      `json:"agentCount"`
	AgentCountBasis              string     `json:"agentCountBasis"`
	Events                       int64      `json:"events"`
	ToolCalls                    int64      `json:"toolCalls"`
	Errors                       int64      `json:"errors"`
	ToolResultsWithUnknownStatus int64      `json:"toolResultsWithUnknownStatus"`
	IndexingErrors               int64      `json:"indexingErrors"`
	FirstActivity                *time.Time `json:"firstActivity"`
	LastActivity                 *time.Time `json:"lastActivity"`
	DurationMS                   *int64     `json:"durationMs"`
}

type AgentSummary struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name,omitempty"`
	Kind               string          `json:"kind"`
	Events             int64           `json:"events"`
	ToolCalls          int64           `json:"toolCalls"`
	Errors             int64           `json:"errors"`
	TokensIn           int64           `json:"tokensIn"`
	TokensCache        int64           `json:"tokensCache"`
	TokensCacheWrite   int64           `json:"tokensCacheWrite"`
	TokensOut          int64           `json:"tokensOut"`
	UnknownModelTokens int64           `json:"unknownModelTokens"`
	CostEstimate       *float64        `json:"costEstimate"`
	Pricing            *SessionPricing `json:"pricing,omitempty"`
}
type AgentPage struct {
	Agents     []AgentSummary `json:"agents"`
	NextCursor string         `json:"nextCursor,omitempty"`
}
type ModelUsage struct {
	Model            string          `json:"model"`
	AgentCount       int64           `json:"agentCount"`
	Observations     int64           `json:"observations"`
	TokensIn         int64           `json:"tokensIn"`
	TokensCache      int64           `json:"tokensCache"`
	TokensCacheWrite int64           `json:"tokensCacheWrite"`
	TokensOut        int64           `json:"tokensOut"`
	Attribution      string          `json:"attribution"`
	CostEstimate     *float64        `json:"costEstimate"`
	Pricing          *SessionPricing `json:"pricing,omitempty"`
}
type ModelUsagePage struct {
	Models     []ModelUsage `json:"models"`
	NextCursor string       `json:"nextCursor,omitempty"`
}
type GroupQuery struct {
	SessionQuery
	Dimension string
}
type UsageGroup struct {
	PricedTokens       int64    `json:"pricedTokens"`
	UnpricedTokens     int64    `json:"unpricedTokens"`
	Key                string   `json:"key"`
	Label              string   `json:"label"`
	Sessions           int64    `json:"sessions"`
	Observations       int64    `json:"observations"`
	TokensIn           int64    `json:"tokensIn"`
	TokensCache        int64    `json:"tokensCache"`
	TokensCacheWrite   int64    `json:"tokensCacheWrite"`
	TokensOut          int64    `json:"tokensOut"`
	UnknownModelTokens *int64   `json:"unknownModelTokens"`
	CostEstimate       *float64 `json:"costEstimate"`
	PricingCoverage    string   `json:"pricingCoverage"`
}
type GroupPage struct {
	Dimension  string       `json:"dimension"`
	Groups     []UsageGroup `json:"groups"`
	NextCursor string       `json:"nextCursor,omitempty"`
}
type CatalogItem struct {
	SourceOnly   bool           `json:"sourceOnly,omitempty"`
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Sessions     int64          `json:"sessions"`
	Accounting   *PricingTotals `json:"accounting,omitempty"`
	CostEstimate *float64       `json:"costEstimate"`
	LastActivity string         `json:"lastActivity,omitempty"`
}
type CatalogPage struct {
	Items      []CatalogItem `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
}
type EventsWindow struct {
	Snapshot       string  `json:"snapshot"`
	Events         []Event `json:"events"`
	BeforeSequence *int64  `json:"beforeSequence"`
	AfterSequence  *int64  `json:"afterSequence"`
	AnchorSequence int64   `json:"anchorSequence"`
}
type SessionReference struct {
	ID       string `json:"id"`
	NativeID string `json:"nativeId"`
	Title    string `json:"title"`
}
type LineagePage struct {
	Parents          []SessionReference `json:"parents"`
	Children         []SessionReference `json:"children"`
	ParentNativeID   string             `json:"parentNativeId"`
	ParentResolution string             `json:"parentResolution"`
	NextCursor       string             `json:"nextCursor,omitempty"`
}

// SetupAnalytics is an atomic, rebuildable read-model migration. SQL triggers
// keep it in the same commit as parser or organization writes. No raw evidence,
// token observation, rate card, or user organization record is changed here.
func (s *Store) SetupAnalytics(ctx context.Context) error {
	if err := s.setupAnalytics(ctx); err != nil {
		return err
	}
	if err := s.ensureModelUsageProjection(ctx); err != nil {
		return err
	}
	if err := s.ensureAgentUsageProjection(ctx); err != nil {
		return err
	}
	if err := s.ensureUsageProjection(ctx, true); err != nil {
		return err
	}
	if err := s.ensureUsageScopeIndex(ctx); err != nil {
		return err
	}
	if err := s.ensureUsageTimeIndex(ctx); err != nil {
		return err
	}
	if err := s.ensureToolMatchIndex(ctx); err != nil {
		return err
	}
	if err := s.ensureDelegatedIndex(ctx); err != nil {
		return err
	}
	if err := s.ensureBehaviorTools(ctx); err != nil {
		return err
	}
	return s.ensureCatalogSearch(ctx)
}
func (s *Store) setupAnalytics(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var version string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='analytics_schema'").Scan(&version)
	if err == nil && version == "10" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && version != "1" && version != "2" && version != "3" && version != "4" && version != "5" && version != "6" && version != "7" && version != "8" && version != "9" {
		return fmt.Errorf("unsupported analytics schema %s", version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == "8" || version == "9" {
		if _, err = tx.ExecContext(ctx, `DROP TRIGGER query_metadata_update; DROP TRIGGER query_metadata_insert;`+metadataCatalogUpdate+metadataCatalogInsert+`UPDATE properties SET value='10' WHERE key='analytics_schema';`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version == "5" || version == "6" || version == "7" {
		for _, name := range []string{"query_session_insert", "query_session_update", "query_session_delete", "query_metadata_insert", "query_metadata_update", "query_metadata_delete"} {
			if _, err = tx.ExecContext(ctx, "DROP TRIGGER "+name); err != nil {
				return err
			}
		}
		if err = setupCatalogTriggers(ctx, tx); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, machineAccountingIndex+`UPDATE properties SET value='10' WHERE key='analytics_schema';`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version == "3" || version == "4" {
		// Add only the narrow catalog field. Do not rebuild historical usage,
		// event aggregates, or search indexes for an organization-only upgrade.
		if version == "3" {
			if _, err = tx.ExecContext(ctx, `ALTER TABLE query_sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
		 UPDATE query_sessions SET pinned=COALESCE((SELECT json_extract(value,'$.pinned') FROM session_metadata WHERE session_id=query_sessions.id),0);`+pinnedCatalogIndexes); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `ALTER TABLE query_sessions ADD COLUMN pricing_known INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE query_sessions ADD COLUMN priced_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE query_sessions ADD COLUMN unattributed_tokens INTEGER NOT NULL DEFAULT 0;
		UPDATE query_sessions SET pricing_known=COALESCE((SELECT json_extract(projection,'$.pricing.snapshotId') IS NOT NULL FROM sessions WHERE id=query_sessions.id),0),
		priced_tokens=COALESCE((SELECT json_extract(projection,'$.pricing.pricedTokens') FROM sessions WHERE id=query_sessions.id),0),
		unattributed_tokens=COALESCE((SELECT json_extract(projection,'$.pricing.unattributedTokens') FROM sessions WHERE id=query_sessions.id),0);`); err != nil {
			return err
		}
		for _, name := range []string{"query_session_insert", "query_session_update", "query_session_delete", "query_metadata_insert", "query_metadata_update", "query_metadata_delete"} {
			if _, err = tx.ExecContext(ctx, "DROP TRIGGER "+name); err != nil {
				return err
			}
		}
		if err = setupCatalogTriggers(ctx, tx); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, machineAccountingIndex+`UPDATE properties SET value='10' WHERE key='analytics_schema'`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version == "1" || version == "2" {
		// These are disposable read models only. Organization/audit records and
		// all raw evidence remain unchanged by this atomic version upgrade.
		if _, err = tx.ExecContext(ctx, analyticsDropV1); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, analyticsSchema+pinnedCatalogIndexes); err != nil {
		return err
	}
	if err = setupCatalogTriggers(ctx, tx); err != nil {
		return err
	}
	usageUpsert := `INSERT INTO query_usage ` + usageSelect("NEW") + ` ON CONFLICT(id) DO UPDATE SET session_id=excluded.session_id,source_id=excluded.source_id,generation=excluded.generation,projection_revision=excluded.projection_revision,
	 agent_id=excluded.agent_id,model=excluded.model,kind=excluded.kind,timestamp=excluded.timestamp,tokens_in=excluded.tokens_in,tokens_cache=excluded.tokens_cache,tokens_write=excluded.tokens_write,tokens_out=excluded.tokens_out;`
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err = tx.ExecContext(ctx, `CREATE TRIGGER query_usage_`+strings.ToLower(event)+` AFTER `+event+` ON usage_observations BEGIN `+usageUpsert+` END;`); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `CREATE TRIGGER query_usage_delete AFTER DELETE ON usage_observations BEGIN DELETE FROM query_usage WHERE id=OLD.id;END;`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TRIGGER query_event_insert AFTER INSERT ON events BEGIN `+eventAddSQL("NEW")+` END;
	 CREATE TRIGGER query_event_delete AFTER DELETE ON events BEGIN `+eventRemoveSQL("OLD")+` END;
	 CREATE TRIGGER query_event_update AFTER UPDATE ON events BEGIN `+eventRemoveSQL("OLD")+eventAddSQL("NEW")+` END;`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO query_sessions (`+catalogColumns+`) `+catalogSelect+`; INSERT INTO query_usage `+usageSelect("u")+` FROM usage_observations u;`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO query_event_agents SELECT session_id,source_id,generation,projection_revision,agent_id,count(*),
	 SUM(kind='tool-call'),COALESCE(SUM(kind='tool-result' AND json_extract(data,'$.error')=1),0),
	 SUM(kind='tool-result' AND (json_extract(data,'$.error') IS NULL OR json_extract(data,'$.error') NOT IN(0,1))),
	 SUM(kind='indexing-error'),MIN(CASE WHEN timestamp>'0001-01-01T00:00:00.000000000Z' THEN timestamp END),MAX(CASE WHEN timestamp>'0001-01-01T00:00:00.000000000Z' THEN timestamp END)
	 FROM events GROUP BY session_id,source_id,generation,projection_revision,agent_id;`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, machineAccountingIndex+`INSERT INTO properties(key,value) VALUES('analytics_schema','10') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return err
	}
	return tx.Commit()
}

func setupCatalogTriggers(ctx context.Context, tx *sql.Tx) error {
	if err := setupNativeAgentColumn(ctx, tx); err != nil {
		return err
	}
	if err := setupEconomicsColumns(ctx, tx); err != nil {
		return err
	}
	upsert := `INSERT INTO query_sessions (` + catalogColumns + `) ` + catalogSelect + ` WHERE s.id=NEW.id ON CONFLICT(id) DO UPDATE SET
	 source_id=excluded.source_id,generation=excluded.generation,projection_revision=excluded.projection_revision,machine_id=excluded.machine_id,provider=excluded.provider,
	 project=excluded.project,title=excluded.title,native_id=excluded.native_id,parent_native_id=excluded.parent_native_id,native_agent_id=excluded.native_agent_id,
	 archived=excluded.archived,pinned=excluded.pinned,pricing_known=excluded.pricing_known,priced_tokens=excluded.priced_tokens,unattributed_tokens=excluded.unattributed_tokens,last_activity=excluded.last_activity,event_count=excluded.event_count,
	 tokens_in=excluded.tokens_in,tokens_cache=excluded.tokens_cache,tokens_write=excluded.tokens_write,tokens_out=excluded.tokens_out,cost_estimate=excluded.cost_estimate,
	 component_known=excluded.component_known,component_tokens=excluded.component_tokens,cost_input=excluded.cost_input,cost_cache_read=excluded.cost_cache_read,cost_cache_write=excluded.cost_cache_write,cost_output=excluded.cost_output;`
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := strings.ToLower(event)
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER query_session_`+name+` AFTER `+event+` ON sessions BEGIN `+upsert+` END;`); err != nil {
			return err
		}
		metadataTrigger := metadataCatalogInsert
		if event == "UPDATE" {
			metadataTrigger = metadataCatalogUpdate
		}
		if _, err := tx.ExecContext(ctx, metadataTrigger); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TRIGGER query_session_delete AFTER DELETE ON sessions BEGIN DELETE FROM query_sessions WHERE id=OLD.id;END;
	 CREATE TRIGGER query_metadata_delete AFTER DELETE ON session_metadata BEGIN `+strings.Replace(upsert, "s.id=NEW.id", "s.id=OLD.session_id", 1)+` END;`); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureAnalytics(ctx context.Context) error {
	// Inspect both durable versions in one snapshot/connection acquisition.
	// Do not cache readiness: migrations and repairs must remain observable.
	var analytics, search sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT
	 (SELECT value FROM properties WHERE key='analytics_schema'),
	 (SELECT value FROM properties WHERE key='catalog_search_schema')`).Scan(&analytics, &search)
	if err != nil {
		return err
	}
	if !analytics.Valid || analytics.String != "10" {
		return s.SetupAnalytics(ctx)
	}
	if search.Valid && search.String == "4" {
		return nil
	}
	return s.ensureCatalogSearch(ctx)
}

// The initial metadata row may arrive before or after its source projection.
// Preserve the missing-row insert, but never rewrite accounting on conflict.
const metadataCatalogInsert = `CREATE TRIGGER query_metadata_insert AFTER INSERT ON session_metadata BEGIN
 INSERT INTO query_sessions (` + catalogColumns + `) ` + catalogSelect + ` WHERE s.id=NEW.session_id
 ON CONFLICT(id) DO UPDATE SET project=excluded.project,title=excluded.title,archived=excluded.archived,pinned=excluded.pinned;
 END;`

// Organization edits cannot change recorded accounting. Update only changed
// owner-controlled fields, avoiding rewrites of unrelated token/cost indexes.
const metadataCatalogUpdate = `CREATE TRIGGER query_metadata_update AFTER UPDATE ON session_metadata BEGIN
 UPDATE query_sessions SET project=CASE WHEN json_extract(NEW.value,'$.projectOverride')=1 THEN COALESCE(json_extract(NEW.value,'$.project'),'') ELSE COALESCE(NULLIF(json_extract(NEW.value,'$.project'),''),(SELECT project FROM sessions WHERE id=NEW.session_id)) END
 WHERE id=NEW.session_id AND (json_extract(OLD.value,'$.project') IS NOT json_extract(NEW.value,'$.project') OR json_extract(OLD.value,'$.projectOverride') IS NOT json_extract(NEW.value,'$.projectOverride'));
 UPDATE query_sessions SET title=COALESCE(NULLIF(json_extract(NEW.value,'$.name'),''),(SELECT COALESCE(json_extract(projection,'$.title'),'') FROM sessions WHERE id=NEW.session_id))
 WHERE id=NEW.session_id AND json_extract(OLD.value,'$.name') IS NOT json_extract(NEW.value,'$.name');
 UPDATE query_sessions SET archived=COALESCE(json_extract(NEW.value,'$.archived'),0)
 WHERE id=NEW.session_id AND json_extract(OLD.value,'$.archived') IS NOT json_extract(NEW.value,'$.archived');
 UPDATE query_sessions SET pinned=COALESCE(json_extract(NEW.value,'$.pinned'),0)
 WHERE id=NEW.session_id AND json_extract(OLD.value,'$.pinned') IS NOT json_extract(NEW.value,'$.pinned');
 END;`

const machineAccountingIndex = `CREATE INDEX IF NOT EXISTS query_machine_accounting ON query_sessions(machine_id,tokens_in,tokens_cache,tokens_write,tokens_out,priced_tokens,unattributed_tokens,pricing_known,cost_estimate,last_activity);`

const catalogColumns = `id,source_id,generation,projection_revision,machine_id,provider,project,title,native_id,parent_native_id,archived,last_activity,event_count,tokens_in,tokens_cache,tokens_write,tokens_out,cost_estimate,pinned,pricing_known,priced_tokens,unattributed_tokens,component_known,component_tokens,cost_input,cost_cache_read,cost_cache_write,cost_output,native_agent_id`

const catalogSelect = `SELECT s.id,s.source_id,s.generation,COALESCE(json_extract(s.projection,'$.projectionRevision'),''),s.machine_id,s.provider,
 CASE WHEN json_extract(m.value,'$.projectOverride')=1 THEN COALESCE(json_extract(m.value,'$.project'),'') ELSE COALESCE(NULLIF(json_extract(m.value,'$.project'),''),s.project) END,
 COALESCE(NULLIF(json_extract(m.value,'$.name'),''),json_extract(s.projection,'$.title'),''),
 COALESCE(json_extract(s.projection,'$.nativeId'),''),COALESCE(json_extract(s.projection,'$.parentThreadId'),''),
 COALESCE(json_extract(m.value,'$.archived'),0),s.last_activity,
 COALESCE(json_extract(s.projection,'$.eventCount'),0),COALESCE(json_extract(s.projection,'$.tokensIn'),0),
 COALESCE(json_extract(s.projection,'$.tokensCache'),0),COALESCE(json_extract(s.projection,'$.tokensCacheWrite'),0),
 COALESCE(json_extract(s.projection,'$.tokensOut'),0),json_extract(s.projection,'$.costEstimate'),COALESCE(json_extract(m.value,'$.pinned'),0),
 (json_extract(s.projection,'$.pricing.snapshotId') IS NOT NULL),COALESCE(json_extract(s.projection,'$.pricing.pricedTokens'),0),COALESCE(json_extract(s.projection,'$.pricing.unattributedTokens'),0),
 (json_extract(s.projection,'$.pricing.components') IS NOT NULL),COALESCE(json_extract(s.projection,'$.pricing.components.pricedTokens'),0),
 COALESCE(json_extract(s.projection,'$.pricing.components.input'),0),COALESCE(json_extract(s.projection,'$.pricing.components.cacheRead'),0),
 COALESCE(json_extract(s.projection,'$.pricing.components.cacheWrite'),0),COALESCE(json_extract(s.projection,'$.pricing.components.output'),0),COALESCE(json_extract(s.projection,'$.nativeAgentId'),'')
 FROM sessions s LEFT JOIN session_metadata m ON m.session_id=s.id`

const pinnedCatalogIndexes = `
CREATE INDEX query_pin_activity ON query_sessions(pinned DESC,last_activity DESC,id);
CREATE INDEX query_pin_activity_asc ON query_sessions(pinned DESC,last_activity,id);
CREATE INDEX query_pin_title ON query_sessions(pinned DESC,title,id);
CREATE INDEX query_pin_title_desc ON query_sessions(pinned DESC,title DESC,id);
CREATE INDEX query_pin_tokens ON query_sessions(pinned DESC,(tokens_in+tokens_cache+tokens_write+tokens_out) DESC,id);
CREATE INDEX query_pin_tokens_asc ON query_sessions(pinned DESC,(tokens_in+tokens_cache+tokens_write+tokens_out),id);
CREATE INDEX query_pin_cost ON query_sessions(pinned DESC,(cost_estimate IS NULL),cost_estimate DESC,id);
CREATE INDEX query_pin_cost_asc ON query_sessions(pinned DESC,(cost_estimate IS NULL),cost_estimate,id);
`

func usageSelect(alias string) string {
	fields := []string{alias + ".id", alias + ".session_id", alias + ".source_id", alias + ".generation", alias + ".projection_revision"}
	for _, key := range []string{"agentId", "model", "kind"} {
		fields = append(fields, "COALESCE(json_extract("+alias+".observation,'$."+key+"'),'')")
	}
	fields = append(fields, "amc_timestamp(json_extract("+alias+".observation,'$.timestamp'))")
	for _, key := range []string{"tokensIn", "tokensCache", "tokensCacheWrite", "tokensOut"} {
		fields = append(fields, "COALESCE(json_extract("+alias+".observation,'$."+key+"'),0)")
	}
	return "SELECT " + strings.Join(fields, ",")
}

const analyticsSchema = `
CREATE TABLE query_sessions(id TEXT PRIMARY KEY,source_id TEXT NOT NULL,generation TEXT NOT NULL,projection_revision TEXT NOT NULL,machine_id TEXT NOT NULL,provider TEXT NOT NULL,
 project TEXT NOT NULL,title TEXT NOT NULL COLLATE NOCASE,native_id TEXT NOT NULL,parent_native_id TEXT NOT NULL,archived INTEGER NOT NULL,
 last_activity TEXT NOT NULL,event_count INTEGER NOT NULL,tokens_in INTEGER NOT NULL,tokens_cache INTEGER NOT NULL,tokens_write INTEGER NOT NULL,tokens_out INTEGER NOT NULL,cost_estimate REAL,pinned INTEGER NOT NULL DEFAULT 0,
 pricing_known INTEGER NOT NULL DEFAULT 0,priced_tokens INTEGER NOT NULL DEFAULT 0,unattributed_tokens INTEGER NOT NULL DEFAULT 0);
CREATE INDEX query_activity ON query_sessions(last_activity DESC,id);
CREATE INDEX query_activity_asc ON query_sessions(last_activity,id);
CREATE INDEX query_title ON query_sessions(title,id);
CREATE INDEX query_title_desc ON query_sessions(title DESC,id);
CREATE INDEX query_tokens ON query_sessions((tokens_in+tokens_cache+tokens_write+tokens_out) DESC,id);
CREATE INDEX query_tokens_asc ON query_sessions((tokens_in+tokens_cache+tokens_write+tokens_out),id);
CREATE INDEX query_cost ON query_sessions((cost_estimate IS NULL),cost_estimate DESC,id);
CREATE INDEX query_cost_asc ON query_sessions((cost_estimate IS NULL),cost_estimate,id);
CREATE INDEX query_machine ON query_sessions(machine_id,last_activity DESC,id);
CREATE INDEX query_provider ON query_sessions(provider,last_activity DESC,id);
CREATE INDEX query_project ON query_sessions(project,last_activity DESC,id);
CREATE INDEX query_archived ON query_sessions(archived,last_activity DESC,id);
CREATE INDEX query_parent ON query_sessions(machine_id,provider,parent_native_id,id);
CREATE INDEX query_native ON query_sessions(machine_id,provider,native_id,id);
CREATE TABLE query_usage(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,source_id TEXT NOT NULL,generation TEXT NOT NULL,projection_revision TEXT NOT NULL,agent_id TEXT NOT NULL,
 model TEXT NOT NULL,kind TEXT NOT NULL,timestamp TEXT NOT NULL,tokens_in INTEGER NOT NULL,tokens_cache INTEGER NOT NULL,tokens_write INTEGER NOT NULL,tokens_out INTEGER NOT NULL);
CREATE INDEX query_usage_session_agent ON query_usage(session_id,generation,projection_revision,agent_id,model,id,source_id,timestamp);
CREATE INDEX query_usage_model ON query_usage(model,session_id,id);
CREATE INDEX query_usage_time ON query_usage(timestamp,id);
CREATE INDEX query_events_scope ON events(session_id,generation,projection_revision,agent_id,seq);
CREATE INDEX query_events_time ON events(session_id,generation,projection_revision,timestamp,seq);
CREATE INDEX query_events_agent_time ON events(session_id,source_id,generation,projection_revision,agent_id,timestamp);
CREATE TABLE query_event_agents(session_id TEXT NOT NULL,source_id TEXT NOT NULL,generation TEXT NOT NULL,projection_revision TEXT NOT NULL,agent_id TEXT NOT NULL,
 events INTEGER NOT NULL,tool_calls INTEGER NOT NULL,errors INTEGER NOT NULL,unknown_results INTEGER NOT NULL,indexing_errors INTEGER NOT NULL,
 first_at TEXT,last_at TEXT,PRIMARY KEY(session_id,source_id,generation,projection_revision,agent_id));
`

const analyticsDropV1 = catalogSearchDrop + `
DROP TRIGGER IF EXISTS query_session_insert; DROP TRIGGER IF EXISTS query_session_update; DROP TRIGGER IF EXISTS query_session_delete;
DROP TRIGGER IF EXISTS query_metadata_insert; DROP TRIGGER IF EXISTS query_metadata_update; DROP TRIGGER IF EXISTS query_metadata_delete;
DROP TRIGGER IF EXISTS query_usage_insert; DROP TRIGGER IF EXISTS query_usage_update; DROP TRIGGER IF EXISTS query_usage_delete;
DROP TRIGGER IF EXISTS query_event_insert; DROP TRIGGER IF EXISTS query_event_update; DROP TRIGGER IF EXISTS query_event_delete;
DROP TABLE IF EXISTS query_sessions; DROP TABLE IF EXISTS query_usage; DROP TABLE IF EXISTS query_event_agents;
DROP INDEX IF EXISTS query_events_scope; DROP INDEX IF EXISTS query_events_time; DROP INDEX IF EXISTS query_events_agent_time;
`

func eventAddSQL(a string) string {
	ts := "CASE WHEN " + a + ".timestamp>'0001-01-01T00:00:00.000000000Z' THEN " + a + ".timestamp END"
	return `INSERT INTO query_event_agents VALUES(` + a + `.session_id,` + a + `.source_id,` + a + `.generation,` + a + `.projection_revision,` + a + `.agent_id,1,
	 (` + a + `.kind='tool-call'),COALESCE((` + a + `.kind='tool-result' AND json_extract(` + a + `.data,'$.error')=1),0),
	 (` + a + `.kind='tool-result' AND (json_extract(` + a + `.data,'$.error') IS NULL OR json_extract(` + a + `.data,'$.error') NOT IN(0,1))),
	 (` + a + `.kind='indexing-error'),` + ts + `,` + ts + `)
	 ON CONFLICT(session_id,source_id,generation,projection_revision,agent_id) DO UPDATE SET events=events+1,tool_calls=tool_calls+excluded.tool_calls,
	 errors=errors+excluded.errors,unknown_results=unknown_results+excluded.unknown_results,indexing_errors=indexing_errors+excluded.indexing_errors,
	 first_at=CASE WHEN first_at IS NULL THEN excluded.first_at WHEN excluded.first_at IS NULL THEN first_at ELSE min(first_at,excluded.first_at) END,
	 last_at=CASE WHEN last_at IS NULL THEN excluded.last_at WHEN excluded.last_at IS NULL THEN last_at ELSE max(last_at,excluded.last_at) END;`
}
func eventRemoveSQL(a string) string {
	match := `session_id=` + a + `.session_id AND source_id=` + a + `.source_id AND generation=` + a + `.generation AND projection_revision=` + a + `.projection_revision AND agent_id=` + a + `.agent_id`
	return `UPDATE query_event_agents SET events=events-1,tool_calls=tool_calls-(` + a + `.kind='tool-call'),
	 errors=errors-COALESCE((` + a + `.kind='tool-result' AND json_extract(` + a + `.data,'$.error')=1),0),
	 unknown_results=unknown_results-(` + a + `.kind='tool-result' AND (json_extract(` + a + `.data,'$.error') IS NULL OR json_extract(` + a + `.data,'$.error') NOT IN(0,1))),
	 indexing_errors=indexing_errors-(` + a + `.kind='indexing-error'),
	 first_at=(SELECT MIN(timestamp) FROM events WHERE ` + match + ` AND timestamp>'0001-01-01T00:00:00.000000000Z'),
	 last_at=(SELECT MAX(timestamp) FROM events WHERE ` + match + ` AND timestamp>'0001-01-01T00:00:00.000000000Z') WHERE ` + match + `;
	 DELETE FROM query_event_agents WHERE ` + match + ` AND events=0;`
}

func catalogWhere(q SessionQuery) (string, []any, error) {
	parts := []string{"1=1"}
	args := []any{}
	if q.MissingUndoEvidence {
		parts = append(parts, `NOT EXISTS (SELECT 1 FROM git_undo_checkpoints h WHERE h.source_id=q.source_id AND h.generation=q.generation AND h.revision=q.projection_revision)`)
	}
	if q.MissingJavaScriptEvidence {
		// Probe the existing primary key, not transcript events. A staged row
		// cannot hide work still missing from the published interpretation.
		parts = append(parts, `NOT EXISTS (SELECT 1 FROM hook_javascript_evidence h WHERE h.source_id=q.source_id AND h.generation=q.generation AND h.revision=q.projection_revision)`)
	}
	for _, filter := range []struct{ value, column string }{{q.MachineID, "q.machine_id"}, {q.Provider, "q.provider"}, {q.Project, "q.project"}} {
		if filter.value != "" {
			parts = append(parts, filter.column+"=?")
			args = append(args, filter.value)
		}
	}
	if q.UnassignedProject {
		if q.Project != "" {
			return "", nil, fmt.Errorf("%w: conflicting project filters", ErrInvalid)
		}
		parts = append(parts, "q.project=''")
	}
	if q.ProjectAssignment != nil {
		if len(*q.ProjectAssignment) > 1024 {
			return "", nil, ErrInvalid
		}
		parts = append(parts, "COALESCE((SELECT json_extract(pm.value,'$.project') FROM session_metadata pm WHERE pm.session_id=q.id),'')=?")
		args = append(args, *q.ProjectAssignment)
	}
	if q.Archived != nil {
		parts = append(parts, "q.archived=?")
		args = append(args, *q.Archived)
	}
	if len(q.Text) > 4096 {
		return "", nil, ErrInvalid
	}
	if q.Text != "" {
		// Legacy table search included project, machine, session identity and
		// owner notes. Keep this one predicate shared by pages and aggregates;
		// do not download the full fleet or filter only the current page.
		predicate, searchArgs := catalogSearchPredicate(q.Text)
		parts = append(parts, `q.rowid IN (SELECT rowid FROM catalog_search WHERE `+predicate+`)`)
		args = append(args, searchArgs...)
	}
	if q.From != nil {
		parts = append(parts, "q.last_activity>=?")
		args = append(args, stamp(*q.From))
	}
	if q.To != nil {
		parts = append(parts, "q.last_activity<?")
		args = append(args, stamp(*q.To))
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return "", nil, ErrInvalid
	}
	return strings.Join(parts, " AND "), args, nil
}

type catalogCursor struct {
	PinnedFirst bool     `json:"pf,omitempty"`
	Pinned      *bool    `json:"p,omitempty"`
	Sort        string   `json:"s,omitempty"`
	Direction   string   `json:"d,omitempty"`
	Text        string   `json:"v,omitempty"`
	Number      *float64 `json:"n,omitempty"`
	Integer     *int64   `json:"t,omitempty"`
	Null        bool     `json:"z,omitempty"`
	ID          string   `json:"i"`
	At          string   `json:"a,omitempty"`
}

func (s *Store) QuerySessions(ctx context.Context, q SessionQuery) (SessionPage, error) {
	page := SessionPage{Sessions: []Session{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return page, err
	}
	filterWhereLength, filterArgCount := len(where), len(args)
	sort := q.Sort
	if sort == "" {
		sort = "lastActivity"
	}
	direction := strings.ToLower(q.Direction)
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		return page, ErrInvalid
	}
	columns := map[string]string{"lastActivity": "q.last_activity", "title": "q.title", "tokens": "(q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out)", "cost": "q.cost_estimate"}
	column, ok := columns[sort]
	if !ok {
		return page, fmt.Errorf("%w: unsupported sort", ErrInvalid)
	}
	if q.Cursor != "" {
		var cursor catalogCursor
		raw, decodeErr := base64.RawURLEncoding.DecodeString(q.Cursor)
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || !validID(cursor.ID) {
			return page, ErrInvalid
		}
		if cursor.Sort == "" && cursor.At != "" {
			cursor.Sort = "lastActivity"
			cursor.Direction = "desc"
			cursor.Text = cursor.At
		}
		if cursor.Sort != sort || cursor.Direction != direction || cursor.PinnedFirst != q.PinnedFirst || (q.PinnedFirst && cursor.Pinned == nil) {
			return page, fmt.Errorf("%w: cursor sort mismatch", ErrInvalid)
		}
		var value any = cursor.Text
		if sort == "tokens" {
			if cursor.Integer == nil {
				return page, ErrInvalid
			}
			value = *cursor.Integer
		} else if sort == "cost" && !cursor.Null {
			if cursor.Number == nil {
				return page, ErrInvalid
			}
			value = *cursor.Number
		} else if cursor.Text == "" && sort == "lastActivity" {
			return page, ErrInvalid
		}
		operator := ">"
		if direction == "desc" {
			operator = "<"
		}
		if q.PinnedFirst {
			where += " AND (q.pinned<? OR (q.pinned=? AND ("
			args = append(args, *cursor.Pinned, *cursor.Pinned)
		} else {
			where += " AND ("
		}
		if sort == "cost" && cursor.Null {
			where += "q.cost_estimate IS NULL AND q.id>?"
			args = append(args, cursor.ID)
		} else {
			extra := ""
			if sort == "cost" {
				extra = "q.cost_estimate IS NULL OR "
			}
			where += extra + column + operator + "? OR (" + column + "=? AND q.id>?)"
			args = append(args, value, value, cursor.ID)
		}
		if q.PinnedFirst {
			where += ")))"
		} else {
			where += ")"
		}
	}
	limit := pageLimit(q.Limit)
	args = append(args, limit+1)
	order := column + " " + direction + ",q.id"
	if sort == "cost" {
		order = "(q.cost_estimate IS NULL)," + order
	}
	if q.PinnedFirst {
		order = "q.pinned DESC," + order
	}
	// Limit the narrow relational catalog before reading projection JSON. In
	// particular, a sort never carries full session bodies through a corpus-wide
	// temporary sort or joins every matching session to its metadata first.
	resultOrder := "p.sort_value " + direction + ",p.id"
	if sort == "cost" {
		resultOrder = "(p.sort_value IS NULL)," + resultOrder
	}
	if q.PinnedFirst {
		resultOrder = "p.pinned DESC," + resultOrder
	}
	pageSQL := `WITH page AS MATERIALIZED (SELECT q.id,q.pinned,` + column + ` AS sort_value FROM query_sessions q WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?) `
	projectionSQL := `SELECT ` + sessionProjectionJSON + `,m.value,p.sort_value,p.pinned,COALESCE(NULLIF(ml.display_name,''),NULLIF(json_extract(mh.heartbeat,'$.name'),''),s.machine_id) FROM page p JOIN sessions s ON s.id=p.id LEFT JOIN session_metadata m ON m.session_id=p.id LEFT JOIN machine_labels ml ON ml.machine_id=s.machine_id LEFT JOIN machines mh ON mh.machine_id=s.machine_id ORDER BY ` + resultOrder
	var reader interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	} = s.db
	if q.Text != "" {
		// Check a bounded prefix in requested sort order. Dense searches can
		// return the first page without materializing every matching FTS row.
		// Sparse searches fall back to the complete indexed query. The shared
		// read snapshot prevents edits between the probe and page from hiding
		// a next page. This never changes matching rules or truncates history.
		tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if e != nil {
			return page, e
		}
		defer tx.Rollback()
		reader = tx
		withoutText := q
		withoutText.Text = ""
		prefixWhere, prefixArgs, e := catalogWhere(withoutText)
		if e != nil {
			return page, e
		}
		prefixWhere += where[filterWhereLength:]
		prefixArgs = append(prefixArgs, args[filterArgCount:len(args)-1]...)
		const searchProbeRows = 2048
		prefixSQL := `WITH candidates AS MATERIALIZED (SELECT q.rowid AS search_rowid,q.id,q.pinned,` + column + ` AS sort_value FROM query_sessions q WHERE ` + prefixWhere + ` ORDER BY ` + order + ` LIMIT 2048)`
		probeArgs := append(append([]any{}, prefixArgs...), strings.ToLower(q.Text), limit+1)
		var matched int
		if e = tx.QueryRowContext(ctx, prefixSQL+` SELECT count(*) FROM (SELECT 1 FROM candidates q CROSS JOIN catalog_search f WHERE f.rowid=q.search_rowid AND instr(f.text,?)>0 LIMIT ?)`, probeArgs...).Scan(&matched); e != nil {
			return page, e
		}
		if limit+1 <= searchProbeRows && matched == limit+1 {
			pageSQL = prefixSQL + `, page AS MATERIALIZED (SELECT q.id,q.pinned,q.sort_value FROM candidates q CROSS JOIN catalog_search f WHERE f.rowid=q.search_rowid AND instr(f.text,?)>0 ORDER BY ` + strings.ReplaceAll(resultOrder, "p.", "q.") + ` LIMIT ?) `
			args = probeArgs
		}
	}
	rows, err := reader.QueryContext(ctx, pageSQL+projectionSQL, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var cursors []catalogCursor
	for rows.Next() {
		var raw []byte
		var meta sql.NullString
		var value any
		var pinned bool
		var machineName string
		if err = rows.Scan(&raw, &meta, &value, &pinned, &machineName); err != nil {
			return page, err
		}
		var item Session
		if err = json.Unmarshal(raw, &item); err != nil {
			return page, err
		}
		item.MachineName = machineName
		if meta.Valid {
			if err = json.Unmarshal([]byte(meta.String), &item.Metadata); err != nil {
				return page, err
			}
		}
		c := catalogCursor{Sort: sort, Direction: direction, ID: item.ID}
		if q.PinnedFirst {
			c.PinnedFirst = true
			c.Pinned = &pinned
		}
		switch v := value.(type) {
		case string:
			c.Text = v
		case int64:
			if sort == "tokens" {
				c.Integer = &v
			} else {
				n := float64(v)
				c.Number = &n
			}
		case float64:
			c.Number = &v
		case []byte:
			c.Text = string(v)
		case nil:
			if sort != "cost" {
				return page, ErrInvalid
			}
			c.Null = true
		default:
			return page, fmt.Errorf("%w: invalid sort value", ErrInvalid)
		}
		page.Sessions = append(page.Sessions, item)
		cursors = append(cursors, c)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Sessions) > limit {
		page.Sessions = page.Sessions[:limit]
		c := cursors[limit-1]
		if sort == "lastActivity" && direction == "desc" && !q.PinnedFirst {
			raw, _ := json.Marshal(sessionCursor{At: c.Text, ID: c.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		} else {
			raw, _ := json.Marshal(c)
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
	}
	return page, nil
}

// Attribution is recorded independently of prices. Use the existing compact
// model summary, restricted to the published source revision, rather than scan
// historical observations. Preserve legacy snapshot evidence without adding it
// a second time to the same recorded missing attribution.
const catalogUnattributed = `MAX(q.unattributed_tokens,COALESCE((SELECT SUM(m.unknown_tokens) FROM query_model_usage m WHERE m.session_id=q.id AND m.source_id=q.source_id AND m.generation=q.generation AND m.projection_revision=q.projection_revision),0))`

func (s *Store) CatalogTotals(ctx context.Context, q SessionQuery) (AggregateTotals, error) {
	var result AggregateTotals
	if err := s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return result, err
	}
	from := "query_sessions q"
	if q.Text != "" {
		// Stream search matches directly into the aggregate instead of building
		// an intermediate rowid set proportional to the matching population.
		text := q.Text
		q.Text = ""
		where, args, err = catalogWhere(q)
		if err != nil {
			return result, err
		}
		predicate, searchArgs := catalogSearchPredicate(text)
		from = "catalog_search CROSS JOIN query_sessions q ON q.rowid=catalog_search.rowid"
		where += " AND " + predicate
		args = append(args, searchArgs...)
	}
	var cost sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(event_count),0),COALESCE(sum(tokens_in),0),COALESCE(sum(tokens_cache),0),COALESCE(sum(tokens_write),0),COALESCE(sum(tokens_out),0),sum(cost_estimate),count(cost_estimate),count(*)-count(cost_estimate),
	COALESCE(sum(priced_tokens),0),COALESCE(sum(`+catalogUnattributed+`),0),COALESCE(sum(pricing_known),0),
	COALESCE(sum(CASE WHEN pricing_known=0 THEN tokens_in+tokens_cache+tokens_write+tokens_out ELSE 0 END),0)
	FROM `+from+` WHERE `+where, args...).Scan(&result.Sessions, &result.Events, &result.TokensIn, &result.TokensCache, &result.TokensCacheWrite, &result.TokensOut, &cost, &result.SessionsWithEstimate, &result.SessionsWithoutEstimate, &result.PricedTokens, &result.KnownUnattributedTokens, &result.SessionsWithPricingCoverage, &result.TokensAwaitingPricing)
	if err != nil {
		return result, err
	}
	if cost.Valid {
		result.CostEstimate = &cost.Float64
	}
	for _, n := range []int64{result.TokensIn, result.TokensCache, result.TokensCacheWrite, result.TokensOut} {
		if n < 0 || n > math.MaxInt64-result.RecordedTokens {
			return result, ErrInvalid
		}
		result.RecordedTokens += n
	}
	if result.PricedTokens < 0 || result.PricedTokens > result.RecordedTokens {
		return result, ErrInvalid
	}
	result.UnpricedTokens = result.RecordedTokens - result.PricedTokens
	if result.RecordedTokens > 0 {
		coverage := float64(result.PricedTokens) / float64(result.RecordedTokens)
		result.TokenPricingCoverage = &coverage
	}
	switch {
	case result.RecordedTokens == 0:
		result.PricingCoverage = "no-recorded-usage"
	case result.TokensAwaitingPricing > 0:
		result.PricingCoverage = "pending-pricing"
	case result.UnpricedTokens > 0:
		result.PricingCoverage = "partially-priced"
	case result.KnownUnattributedTokens > 0:
		result.PricingCoverage = "priced-with-incomplete-attribution"
	default:
		result.PricingCoverage = "all-recorded-usage-priced"
	}
	comparison, err := s.comparisonTotals(ctx, from, where, args)
	if err != nil {
		return result, err
	}
	result.CurrentComparisons = &comparison
	return result, nil
}

func (s *Store) SessionStats(ctx context.Context, id string) (SessionStatistics, error) {
	result := SessionStatistics{SessionID: id, AgentCountBasis: "distinct recorded agent scopes; linked child sessions are separate records"}
	if err := s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM query_sessions WHERE id=?", id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	} else if err != nil {
		return result, err
	}
	var first, last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.events),0),COALESCE(SUM(a.tool_calls),0),COALESCE(SUM(a.errors),0),COALESCE(SUM(a.unknown_results),0),COALESCE(SUM(a.indexing_errors),0),MIN(a.first_at),MAX(a.last_at)
	 FROM query_event_agents a JOIN query_sessions q ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation AND q.projection_revision=a.projection_revision WHERE q.id=?`, id).Scan(&result.Events, &result.ToolCalls, &result.Errors, &result.ToolResultsWithUnknownStatus, &result.IndexingErrors, &first, &last)
	if err != nil {
		return result, err
	}
	err = s.db.QueryRowContext(ctx, sessionAgentCountSQL, id, id).Scan(&result.AgentCount)
	if err != nil {
		return result, err
	}
	var usageFirst, usageLast sql.NullString
	if err = s.db.QueryRowContext(ctx, sessionUsageBoundsSQL, id).Scan(&usageFirst, &usageLast); err != nil {
		return result, err
	}
	if usageFirst.Valid && (!first.Valid || usageFirst.String < first.String) {
		first = usageFirst
	}
	if usageLast.Valid && (!last.Valid || usageLast.String > last.String) {
		last = usageLast
	}
	if first.Valid {
		v, e := time.Parse(time.RFC3339Nano, first.String)
		if e != nil {
			return result, e
		}
		result.FirstActivity = &v
	}
	if last.Valid {
		v, e := time.Parse(time.RFC3339Nano, last.String)
		if e != nil {
			return result, e
		}
		result.LastActivity = &v
	}
	if result.FirstActivity != nil && result.LastActivity != nil {
		duration := result.LastActivity.Sub(*result.FirstActivity).Milliseconds()
		result.DurationMS = &duration
	}
	return result, nil
}

type keyCursor struct {
	Key       string `json:"k"`
	Dimension string `json:"d"`
}

func decodeKey(cursor, dimension string) (string, bool, error) {
	if cursor == "" {
		return "", false, nil
	}
	var c keyCursor
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Dimension != dimension || len(c.Key) > 4096 {
		return "", false, ErrInvalid
	}
	return c.Key, true, nil
}
func encodeKey(key, dimension string) string {
	b, _ := json.Marshal(keyCursor{Key: key, Dimension: dimension})
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) SessionAgents(ctx context.Context, id, cursor string, limit int) (AgentPage, error) {
	page := AgentPage{Agents: []AgentSummary{}}
	token, err := s.historySnapshot(ctx, id, "")
	if err != nil {
		return page, err
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	after, has, err := decodeKey(cursor, "agent")
	if err != nil {
		return page, err
	}
	limit = pageLimit(limit)
	var parent, provider, nativeAgent string
	if err = s.db.QueryRowContext(ctx, "SELECT parent_native_id,provider,native_agent_id FROM query_sessions WHERE id=?", id).Scan(&parent, &provider, &nativeAgent); errors.Is(err, sql.ErrNoRows) {
		return page, ErrNotFound
	} else if err != nil {
		return page, err
	}
	query := `WITH e AS(SELECT a.agent_id,a.events,a.tool_calls,a.errors FROM query_event_agents a JOIN query_sessions q ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation AND q.projection_revision=a.projection_revision WHERE q.id=?),
	 u AS(SELECT a.agent_id,SUM(a.tokens_in) AS ti,SUM(a.tokens_cache) AS tc,SUM(a.tokens_write) AS tw,SUM(a.tokens_out) AS tout,
	 SUM(CASE WHEN a.model='' OR a.kind='incomplete-attribution' THEN a.tokens_in+a.tokens_cache+a.tokens_write+a.tokens_out ELSE 0 END) AS unknown
	 FROM query_usage a JOIN query_sessions q ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation AND q.projection_revision=a.projection_revision WHERE q.id=? GROUP BY a.agent_id),
	 ids AS(SELECT agent_id FROM e UNION SELECT agent_id FROM u)
	 SELECT ids.agent_id,COALESCE(e.events,0),COALESCE(e.tool_calls,0),COALESCE(e.errors,0),COALESCE(u.ti,0),COALESCE(u.tc,0),COALESCE(u.tw,0),COALESCE(u.tout,0),COALESCE(u.unknown,0),COALESCE((SELECT name FROM agent_names n WHERE n.session_id=? AND n.agent_id=ids.agent_id),'')
	 FROM ids LEFT JOIN e USING(agent_id) LEFT JOIN u USING(agent_id)`
	args := []any{id, id, id}
	if has {
		query += " WHERE ids.agent_id>?"
		args = append(args, after)
	}
	query += " ORDER BY ids.agent_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AgentSummary
		if err = rows.Scan(&a.ID, &a.Events, &a.ToolCalls, &a.Errors, &a.TokensIn, &a.TokensCache, &a.TokensCacheWrite, &a.TokensOut, &a.UnknownModelTokens, &a.Name); err != nil {
			return page, err
		}
		a.Kind = "unclassified"
		if a.ID == "" {
			a.Kind = "unattributed"
		} else if provider == "claude" && nativeAgent != "" && a.ID == nativeAgent {
			a.Kind = "child-agent"
		} else if a.ID == "main" {
			a.Kind = "main"
			if parent != "" {
				a.Kind = "child-thread"
			}
		}
		page.Agents = append(page.Agents, a)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Agents) > limit {
		page.Agents = page.Agents[:limit]
		page.NextCursor = encodeKey(page.Agents[limit-1].ID, "agent")
	}
	if err = rows.Close(); err != nil {
		return page, err
	}
	keys := make([]string, len(page.Agents))
	for i, a := range page.Agents {
		keys[i] = a.ID
	}
	prices, err := s.CurrentAttributionPrices(ctx, id, "agent", keys)
	if err != nil {
		return page, err
	}
	for i := range page.Agents {
		a := &page.Agents[i]
		if p, ok := prices[a.ID]; ok && p.RecordedTokens == a.TokensIn+a.TokensCache+a.TokensCacheWrite+a.TokensOut {
			a.Pricing = &p
			if p.PricedTokens > 0 {
				a.CostEstimate = &p.Cost
			}
		}
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return AgentPage{}, err
	}
	return page, nil
}

func (s *Store) SessionModelUsage(ctx context.Context, id, cursor string, limit int) (ModelUsagePage, error) {
	page := ModelUsagePage{Models: []ModelUsage{}}
	token, err := s.historySnapshot(ctx, id, "")
	if err != nil {
		return page, err
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	after, has, err := decodeKey(cursor, "model")
	if err != nil {
		return page, err
	}
	limit = pageLimit(limit)
	where := "q.id=?"
	args := []any{id}
	if has {
		where += " AND u.model>?"
		args = append(args, after)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT u.model,count(DISTINCT u.agent_id),count(*),sum(u.tokens_in),sum(u.tokens_cache),sum(u.tokens_write),sum(u.tokens_out),MAX(u.kind='incomplete-attribution')
	 FROM query_usage u JOIN query_sessions q ON q.id=u.session_id AND q.source_id=u.source_id AND q.generation=u.generation AND q.projection_revision=u.projection_revision WHERE `+where+` GROUP BY u.model ORDER BY u.model LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var m ModelUsage
		var incomplete bool
		if err = rows.Scan(&m.Model, &m.AgentCount, &m.Observations, &m.TokensIn, &m.TokensCache, &m.TokensCacheWrite, &m.TokensOut, &incomplete); err != nil {
			return page, err
		}
		m.Attribution = "recorded-model"
		if m.Model == "" || incomplete {
			m.Attribution = "incomplete"
		}
		page.Models = append(page.Models, m)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Models) > limit {
		page.Models = page.Models[:limit]
		page.NextCursor = encodeKey(page.Models[limit-1].Model, "model")
	}
	if err = rows.Close(); err != nil {
		return page, err
	}
	keys := make([]string, len(page.Models))
	for i, m := range page.Models {
		keys[i] = m.Model
	}
	prices, err := s.CurrentAttributionPrices(ctx, id, "model", keys)
	if err != nil {
		return page, err
	}
	for i := range page.Models {
		m := &page.Models[i]
		if p, ok := prices[m.Model]; ok && p.RecordedTokens == m.TokensIn+m.TokensCache+m.TokensCacheWrite+m.TokensOut {
			m.Pricing = &p
			if p.PricedTokens > 0 {
				m.CostEstimate = &p.Cost
			}
		}
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return ModelUsagePage{}, err
	}
	return page, nil
}

func (s *Store) GroupedUsage(ctx context.Context, q GroupQuery) (GroupPage, error) {
	page := GroupPage{Dimension: q.Dimension, Groups: []UsageGroup{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return page, err
	}
	dimensions := map[string]string{"project": "q.project", "machine": "q.machine_id", "provider": "q.provider", "model": "COALESCE(u.model,'')",
		"day":       "CASE WHEN u.timestamp>'0001-01-01T00:00:00.000000000Z' THEN substr(u.timestamp,1,10) ELSE '' END",
		"week":      "CASE WHEN u.timestamp>'0001-01-01T00:00:00.000000000Z' THEN COALESCE(date(substr(u.timestamp,1,10),'weekday 0','-6 days'),'') ELSE '' END",
		"month":     "CASE WHEN u.timestamp>'0001-01-01T00:00:00.000000000Z' THEN substr(u.timestamp,1,7) ELSE '' END",
		"year":      "CASE WHEN u.timestamp>'0001-01-01T00:00:00.000000000Z' THEN substr(u.timestamp,1,4) ELSE '' END",
		"agentKind": "CASE WHEN u.agent_id IS NULL OR u.agent_id='' THEN 'unattributed' WHEN q.provider='claude' AND q.native_agent_id<>'' AND u.agent_id=q.native_agent_id THEN 'child-agent' WHEN u.agent_id='main' AND q.parent_native_id<>'' THEN 'child-thread' WHEN u.agent_id='main' THEN 'main' ELSE 'unclassified' END"}
	dimension, ok := dimensions[q.Dimension]
	if !ok {
		return page, fmt.Errorf("%w: unsupported grouping", ErrInvalid)
	}
	filters := q.SessionQuery
	filters.From = nil
	filters.To = nil
	where, args, err := catalogWhere(filters)
	if err != nil {
		return page, err
	}
	if q.From != nil {
		where += " AND u.timestamp>=?"
		args = append(args, stamp(*q.From))
	}
	if q.To != nil {
		where += " AND u.timestamp<?"
		args = append(args, stamp(*q.To))
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return page, ErrInvalid
	}
	after, has, err := decodeKey(q.Cursor, q.Dimension)
	if err != nil {
		return page, err
	}
	if has {
		where += " AND (" + dimension + ")>?"
		args = append(args, after)
	}
	limit := pageLimit(q.Limit)
	args = append(args, limit+1)
	// Extract published snapshot identities once per priced session, not once
	// per usage row. Unpriced sessions need no wide projection/JSON reads.
	prefix := `WITH selected_prices AS MATERIALIZED (
 SELECT q.id AS session_id,a.id FROM query_sessions q
 JOIN sessions selected ON selected.id=q.id
 JOIN accounting_estimates a ON a.id=json_extract(selected.projection,'$.pricing.snapshotId') AND a.session_id=q.id AND json_extract(a.estimate,'$.attributionVersion')=1
 WHERE q.pricing_known=1)`
	if q.Dimension == "model" {
		// Bound expensive price/projection joins to the requested model page.
		// The key catalog still covers the complete filtered observation history.
		prefix += `, chosen_models AS MATERIALIZED (SELECT DISTINCT COALESCE(u.model,'') AS model
FROM query_sessions q LEFT JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
WHERE ` + where + ` ORDER BY model LIMIT ?) `
		args = append(append([]any{}, args...), args...)
		where += " AND COALESCE(u.model,'') IN (SELECT model FROM chosen_models)"
	}
	prefix += " "
	var rows *sql.Rows
	if q.Dimension != "agentKind" && q.From == nil && q.To == nil {
		rows, err = s.modelUsageRows(ctx, q)
	} else {
		rows, err = s.db.QueryContext(ctx, prefix+`SELECT `+dimension+`,COUNT(DISTINCT q.id),COUNT(u.id),COALESCE(SUM(u.tokens_in),0),COALESCE(SUM(u.tokens_cache),0),COALESCE(SUM(u.tokens_write),0),COALESCE(SUM(u.tokens_out),0),
	 COALESCE(SUM(CASE WHEN u.model='' OR u.kind='incomplete-attribution' THEN u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out ELSE 0 END),0),
	 COALESCE(SUM(json_extract(p.evidence,'$.pricedTokens')),0),SUM(CASE WHEN json_extract(p.evidence,'$.pricedTokens')>0 THEN json_extract(p.evidence,'$.cost') END)
	 FROM query_sessions q LEFT JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
	 LEFT JOIN selected_prices a ON a.session_id=q.id
	 LEFT JOIN observation_prices p ON p.snapshot_id=a.id AND p.observation_id=u.id AND p.agent_id=u.agent_id AND p.model=u.model AND json_extract(p.evidence,'$.recordedTokens')=u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out
	 WHERE `+where+` GROUP BY `+dimension+` ORDER BY `+dimension+` LIMIT ?`, args...)
	}
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var g UsageGroup
		var unknown int64
		var cost sql.NullFloat64
		if err = rows.Scan(&g.Key, &g.Sessions, &g.Observations, &g.TokensIn, &g.TokensCache, &g.TokensCacheWrite, &g.TokensOut, &unknown, &g.PricedTokens, &cost); err != nil {
			return page, err
		}
		g.UnknownModelTokens = &unknown
		g.Label = g.Key
		if g.Label == "" {
			switch q.Dimension {
			case "model":
				g.Label = "Unknown model"
			case "day", "week", "month", "year":
				g.Label = "Undated usage"
			case "project":
				g.Label = "Unassigned project"
			default:
				g.Label = "Unknown"
			}
		}
		if cost.Valid {
			g.CostEstimate = &cost.Float64
		}
		recorded := g.TokensIn + g.TokensCache + g.TokensCacheWrite + g.TokensOut
		g.UnpricedTokens = recorded - g.PricedTokens
		if g.UnpricedTokens < 0 {
			return GroupPage{}, ErrInvalid
		}
		switch {
		case recorded == 0:
			g.PricingCoverage = "no-recorded-usage"
		case g.PricedTokens == 0:
			g.PricingCoverage = "pending-pricing"
		case g.UnpricedTokens > 0:
			g.PricingCoverage = "partially-priced"
		case unknown > 0:
			g.PricingCoverage = "priced-with-incomplete-attribution"
		default:
			g.PricingCoverage = "all-recorded-usage-priced"
		}
		page.Groups = append(page.Groups, g)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Groups) > limit {
		page.Groups = page.Groups[:limit]
		page.NextCursor = encodeKey(page.Groups[limit-1].Key, q.Dimension)
	}
	return page, nil
}

func (s *Store) catalogItems(ctx context.Context, dimension string, q SessionQuery) (CatalogPage, error) {
	page := CatalogPage{Items: []CatalogItem{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	column := "q.project"
	if dimension == "machine" {
		column = "q.machine_id"
	} else if dimension != "project" {
		return page, ErrInvalid
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return page, err
	}
	after, has, err := decodeKey(q.Cursor, dimension)
	if err != nil {
		return page, err
	}
	if has {
		where += " AND " + column + ">?"
		args = append(args, after)
	}
	limit := pageLimit(q.Limit)
	args = append(args, limit+1)
	extra := ""
	if dimension == "machine" {
		extra = `,sum(tokens_in),sum(tokens_cache),sum(tokens_write),sum(tokens_out),sum(priced_tokens),sum(` + catalogUnattributed + `),sum(pricing_known),sum(CASE WHEN pricing_known=0 THEN tokens_in+tokens_cache+tokens_write+tokens_out ELSE 0 END),sum(cost_estimate),max(last_activity)`
		extra += `,COALESCE(NULLIF((SELECT display_name FROM machine_labels ml WHERE ml.machine_id=q.machine_id),''),NULLIF((SELECT json_extract(heartbeat,'$.name') FROM machines m WHERE m.machine_id=q.machine_id),''),q.machine_id)`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+column+`,count(*)`+extra+` FROM query_sessions q WHERE `+where+` GROUP BY `+column+` ORDER BY `+column+` LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item CatalogItem
		var counts [4]int64
		var cost sql.NullFloat64
		dest := []any{&item.ID, &item.Sessions}
		if dimension == "machine" {
			item.Accounting = &PricingTotals{}
			p := item.Accounting
			dest = append(dest, &counts[0], &counts[1], &counts[2], &counts[3], &p.PricedTokens, &p.KnownUnattributedTokens, &p.SessionsWithPricingCoverage, &p.TokensAwaitingPricing, &cost, &item.LastActivity, &item.Name)
		}
		if err = rows.Scan(dest...); err != nil {
			return page, err
		}
		if p := item.Accounting; p != nil {
			for _, n := range counts {
				if n < 0 || n > math.MaxInt64-p.RecordedTokens {
					return page, ErrInvalid
				}
				p.RecordedTokens += n
			}
			if p.PricedTokens < 0 || p.PricedTokens > p.RecordedTokens {
				return page, ErrInvalid
			}
			p.UnpricedTokens = p.RecordedTokens - p.PricedTokens
			if p.RecordedTokens > 0 {
				coverage := float64(p.PricedTokens) / float64(p.RecordedTokens)
				p.TokenPricingCoverage = &coverage
			}
			if cost.Valid {
				item.CostEstimate = &cost.Float64
			}
		}
		if dimension != "machine" {
			item.Name = item.ID
		}
		if item.ID == "" {
			item.Name = "Unassigned"
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeKey(page.Items[limit-1].ID, dimension)
	}
	return page, nil
}
func (s *Store) CatalogProjects(ctx context.Context, q SessionQuery) (CatalogPage, error) {
	return s.catalogItems(ctx, "project", q)
}
func (s *Store) CatalogMachines(ctx context.Context, q SessionQuery) (CatalogPage, error) {
	page, err := s.catalogItems(ctx, "machine", q)
	if err != nil {
		return page, err
	}
	filters := q
	filters.Cursor = ""
	filters.Limit = 0
	filters.Sort = ""
	filters.Direction = ""
	if filters != (SessionQuery{}) {
		return page, nil
	}
	return s.includeSourceOnlyMachines(ctx, q, page)
}

// EventsAround selects by the durable global event sequence, not an array
// position. Sparse sequences and source reindexing cannot target the wrong row.
func (s *Store) EventsAround(ctx context.Context, id string, anchor int64, before, after int) (EventsWindow, error) {
	return s.EventsAroundPinned(ctx, id, anchor, before, after, "")
}

func (s *Store) eventsAround(ctx context.Context, id string, anchor int64, before, after int) (EventsWindow, error) {
	result := EventsWindow{Events: []Event{}, AnchorSequence: anchor}
	if err := s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	if anchor < 0 || before < 0 || after < 0 {
		return result, ErrInvalid
	}
	before = min(before, 249)
	after = min(after, 249)
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT e.seq FROM events e JOIN query_sessions i ON i.id=e.session_id AND i.source_id=e.source_id AND i.generation=e.generation AND i.projection_revision=e.projection_revision WHERE e.session_id=? AND e.seq=?`, id, anchor).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	left, err := s.queryEventsDirectional(ctx, id, anchor, before+1, true)
	if err != nil {
		return result, err
	}
	right, err := s.ListEvents(ctx, id, anchor, after+1)
	if err != nil {
		return result, err
	}
	if len(left) > before {
		cursor := left[before]
		result.BeforeSequence = &cursor
		left = left[:before]
	}
	start := anchor - 1
	if len(left) > 0 {
		start = left[len(left)-1] - 1
	}
	page, err := s.ListEvents(ctx, id, start, len(left)+1)
	if err != nil {
		return result, err
	}
	result.Events = append(result.Events, page.Events...)
	if len(right.Events) > after {
		cursor := right.Events[after].Sequence
		result.AfterSequence = &cursor
		right.Events = right.Events[:after]
	}
	result.Events = append(result.Events, right.Events...)
	return result, nil
}
func (s *Store) queryEventsDirectional(ctx context.Context, id string, anchor int64, limit int, backward bool) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.seq FROM events e JOIN query_sessions i ON i.id=e.session_id AND i.source_id=e.source_id AND i.generation=e.generation AND i.projection_revision=e.projection_revision WHERE e.session_id=? AND e.seq<? ORDER BY e.seq DESC LIMIT ?`, id, anchor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err = rows.Scan(&seq); err != nil {
			return nil, err
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}

func (s *Store) SessionLineage(ctx context.Context, id, cursor string, limit int) (LineagePage, error) {
	page := LineagePage{Parents: []SessionReference{}, Children: []SessionReference{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	var machine, provider, native string
	err := s.db.QueryRowContext(ctx, "SELECT machine_id,provider,native_id,parent_native_id FROM query_sessions WHERE id=?", id).Scan(&machine, &provider, &native, &page.ParentNativeID)
	if errors.Is(err, sql.ErrNoRows) {
		return page, ErrNotFound
	}
	if err != nil {
		return page, err
	}
	page.ParentResolution = "root"
	if page.ParentNativeID != "" {
		rows, err := s.db.QueryContext(ctx, "SELECT id,native_id,title FROM query_sessions WHERE machine_id=? AND provider=? AND native_id=? ORDER BY id LIMIT 100", machine, provider, page.ParentNativeID)
		if err != nil {
			return page, err
		}
		for rows.Next() {
			var r SessionReference
			if err = rows.Scan(&r.ID, &r.NativeID, &r.Title); err != nil {
				rows.Close()
				return page, err
			}
			page.Parents = append(page.Parents, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return page, err
		}
		page.ParentResolution = "unresolved"
		if len(page.Parents) == 1 {
			page.ParentResolution = "unique"
		} else if len(page.Parents) > 1 {
			page.ParentResolution = "ambiguous"
		}
	}
	after, has, err := decodeKey(cursor, "children")
	if err != nil {
		return page, err
	}
	if !has {
		after = ""
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, "SELECT id,native_id,title FROM query_sessions WHERE machine_id=? AND provider=? AND parent_native_id=? AND parent_native_id<>'' AND id>? ORDER BY id LIMIT ?", machine, provider, native, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var r SessionReference
		if err = rows.Scan(&r.ID, &r.NativeID, &r.Title); err != nil {
			return page, err
		}
		page.Children = append(page.Children, r)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Children) > limit {
		page.Children = page.Children[:limit]
		page.NextCursor = encodeKey(page.Children[limit-1].ID, "children")
	}
	return page, nil
}
