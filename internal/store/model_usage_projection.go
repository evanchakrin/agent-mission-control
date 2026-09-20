package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func usageDay(timestamp string) string {
	return "CASE WHEN " + timestamp + ">'0001-01-01T00:00:00.000000000Z' THEN substr(" + timestamp + ",1,10) ELSE '' END"
}

func (s *Store) modelUsageRows(ctx context.Context, q GroupQuery) (*sql.Rows, error) {
	dimension, ok := map[string]string{"model": "COALESCE(m.model,'')", "provider": "q.provider", "machine": "q.machine_id", "project": "q.project",
		"day": "COALESCE(m.day,'')", "week": "COALESCE(date(m.day,'weekday 0','-6 days'),'')", "month": "COALESCE(substr(m.day,1,7),'')", "year": "COALESCE(substr(m.day,1,4),'')"}[q.Dimension]
	if !ok {
		return nil, ErrInvalid
	}
	table, column, priceGroup := "query_model_usage", "model", "u.model"
	if q.Dimension == "day" || q.Dimension == "week" || q.Dimension == "month" || q.Dimension == "year" {
		table, column, priceGroup = "query_daily_usage", "day", usageDay("u.timestamp")
	}
	where, args, err := catalogWhere(q.SessionQuery)
	if err != nil {
		return nil, err
	}
	after, has, err := decodeKey(q.Cursor, q.Dimension)
	if err != nil {
		return nil, err
	}
	if has {
		where += " AND " + dimension + ">?"
		args = append(args, after)
	}
	args = append(args, pageLimit(q.Limit)+1)
	return s.db.QueryContext(ctx, `WITH selected_prices AS MATERIALIZED (
 SELECT q.id AS session_id,a.id FROM query_sessions q JOIN sessions selected ON selected.id=q.id
 JOIN accounting_estimates a ON a.id=json_extract(selected.projection,'$.pricing.snapshotId') AND a.session_id=q.id AND json_extract(a.estimate,'$.attributionVersion')=1 WHERE q.pricing_known=1),
 prices AS MATERIALIZED (
 SELECT u.session_id,`+priceGroup+` AS bucket,SUM(json_extract(p.evidence,'$.pricedTokens')) AS priced,SUM(CASE WHEN json_extract(p.evidence,'$.pricedTokens')>0 THEN json_extract(p.evidence,'$.cost') END) AS cost
 FROM selected_prices a JOIN query_sessions current ON current.id=a.session_id
 JOIN query_usage u ON u.session_id=current.id AND u.source_id=current.source_id AND u.generation=current.generation AND u.projection_revision=current.projection_revision
 JOIN observation_prices p ON p.snapshot_id=a.id AND p.observation_id=u.id AND p.agent_id=u.agent_id AND p.model=u.model AND json_extract(p.evidence,'$.recordedTokens')=u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out
 GROUP BY u.session_id,`+priceGroup+`)
 SELECT `+dimension+`,COUNT(DISTINCT q.id),COALESCE(SUM(m.observations),0),COALESCE(SUM(m.tokens_in),0),COALESCE(SUM(m.tokens_cache),0),COALESCE(SUM(m.tokens_write),0),COALESCE(SUM(m.tokens_out),0),COALESCE(SUM(m.unknown_tokens),0),COALESCE(SUM(p.priced),0),SUM(p.cost)
 FROM query_sessions q LEFT JOIN `+table+` m ON m.session_id=q.id AND m.source_id=q.source_id AND m.generation=q.generation AND m.projection_revision=q.projection_revision
 LEFT JOIN prices p ON p.session_id=q.id AND p.bucket=m.`+column+`
 WHERE `+where+` GROUP BY `+dimension+` ORDER BY `+dimension+` LIMIT ?`, args...)
}

const modelUsageKeys = "session_id,source_id,generation,projection_revision,model"
const modelUsageFields = "observations,tokens_in,tokens_cache,tokens_write,tokens_out,unknown_tokens"

// This rebuildable read model has one row per source revision/model rather than
// per observation. Trigger updates commit with query_usage, including revisions
// to existing provider messages. It contains no prices or organization state.
func (s *Store) ensureModelUsageProjection(ctx context.Context) error {
	return s.ensureUsageProjection(ctx, false)
}

// Identifiers and expressions below are internal constants, never request data.
// Both read models share the same guarded, atomic backfill and delta semantics.
func (s *Store) ensureUsageProjection(ctx context.Context, daily bool) error {
	table, column, prefix := "query_model_usage", "model", "model_usage_"
	expression := func(ref string) string { return ref + ".model" }
	if daily {
		table, column, prefix = "query_daily_usage", "day", "daily_usage_"
		expression = func(ref string) string { return usageDay(ref + ".timestamp") }
	}
	return s.ensureUsageDimension(ctx, table, column, prefix, expression, false)
}

func (s *Store) ensureAgentUsageProjection(ctx context.Context) error {
	return s.ensureUsageDimension(ctx, "query_agent_usage", "agent_id", "agent_usage_", func(ref string) string { return ref + ".agent_id" }, true)
}

func (s *Store) ensureUsageDimension(ctx context.Context, table, column, prefix string, expression func(string) string, includePartial bool) error {
	index := table + "_" + column
	groupKeys := "session_id,source_id,generation,projection_revision," + column
	groupSelect := "session_id,source_id,generation,projection_revision," + expression("query_usage")
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var objects int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE (type='table' AND name=?) OR (type='index' AND name=?) OR (type='trigger' AND name IN (?,?,?))`, table, index, prefix+"insert", prefix+"update", prefix+"delete").Scan(&objects); err != nil {
		return err
	}
	if objects == 5 {
		return nil
	}
	checkSpace := func() error {
		s.capacityMu.Lock()
		defer s.capacityMu.Unlock()
		// Leave room above the existing volume reserve for the next bounded
		// batch's table/index/WAL work. This does not reserve external disk use.
		return s.capacity(128 << 20)
	}
	if err := checkSpace(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+prefix+`insert;DROP TRIGGER IF EXISTS `+prefix+`update;DROP TRIGGER IF EXISTS `+prefix+`delete;
 CREATE TABLE IF NOT EXISTS `+table+`(session_id TEXT NOT NULL,source_id TEXT NOT NULL,generation TEXT NOT NULL,projection_revision TEXT NOT NULL,`+column+` TEXT NOT NULL,
 observations INTEGER NOT NULL CHECK(observations>=0),tokens_in INTEGER NOT NULL,tokens_cache INTEGER NOT NULL,tokens_write INTEGER NOT NULL,tokens_out INTEGER NOT NULL,unknown_tokens INTEGER NOT NULL,
 PRIMARY KEY(`+groupKeys+`));
 CREATE INDEX IF NOT EXISTS `+index+` ON `+table+`(`+column+`,session_id);
 DELETE FROM `+table+`;`); err != nil {
		return err
	}
	keys := strings.Split(groupKeys, ",")
	values := make([]string, len(keys))
	predicates := make([]string, len(keys))
	for i, k := range keys {
		values[i] = "NEW." + k
		predicates[i] = k + "=OLD." + k
		if k == column {
			values[i] = expression("NEW")
			predicates[i] = k + "=" + expression("OLD")
		}
	}
	unknown := func(ref string) string {
		if includePartial {
			return fmt.Sprintf("CASE WHEN %s.model='' OR %s.kind IN ('incomplete-attribution','message-without-id','message-partial') THEN %s.tokens_in+%s.tokens_cache+%s.tokens_write+%s.tokens_out ELSE 0 END", ref, ref, ref, ref, ref, ref)
		}
		return fmt.Sprintf("CASE WHEN %s.model='' OR %s.kind='incomplete-attribution' THEN %s.tokens_in+%s.tokens_cache+%s.tokens_write+%s.tokens_out ELSE 0 END", ref, ref, ref, ref, ref, ref)
	}
	add := `INSERT INTO ` + table + ` (` + groupKeys + `,` + modelUsageFields + `) VALUES (` + strings.Join(values, ",") + `,1,NEW.tokens_in,NEW.tokens_cache,NEW.tokens_write,NEW.tokens_out,` + unknown("NEW") + `) ON CONFLICT (` + groupKeys + `) DO UPDATE SET `
	var updates []string
	for _, field := range strings.Split(modelUsageFields, ",") {
		updates = append(updates, field+"="+field+"+excluded."+field)
	}
	add += strings.Join(updates, ",") + ";"
	// One transaction protects publication; bounded rowid ranges prevent a
	// corpus-sized sort and permit cancellation/storage checks between batches.
	var after sql.NullInt64
	for {
		if err = checkSpace(); err != nil {
			return err
		}
		predicate := ""
		var args []any
		if after.Valid {
			predicate = " WHERE rowid>?"
			args = append(args, after.Int64)
		}
		var upper sql.NullInt64
		if err = tx.QueryRowContext(ctx, "SELECT MAX(rowid) FROM (SELECT rowid FROM query_usage"+predicate+" ORDER BY rowid LIMIT 5000)", args...).Scan(&upper); err != nil {
			return err
		}
		if !upper.Valid {
			break
		}
		if after.Valid {
			predicate += " AND rowid<=?"
		} else {
			predicate = " WHERE rowid<=?"
		}
		args = append(args, upper.Int64)
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+table+` SELECT `+groupSelect+`,COUNT(*),SUM(tokens_in),SUM(tokens_cache),SUM(tokens_write),SUM(tokens_out),SUM(`+unknown("query_usage")+`) FROM query_usage`+predicate+` GROUP BY `+groupSelect+` ON CONFLICT (`+groupKeys+`) DO UPDATE SET `+strings.Join(updates, ","), args...); err != nil {
			return err
		}
		after = upper
	}
	remove := `UPDATE ` + table + ` SET observations=observations-1,tokens_in=tokens_in-OLD.tokens_in,tokens_cache=tokens_cache-OLD.tokens_cache,tokens_write=tokens_write-OLD.tokens_write,tokens_out=tokens_out-OLD.tokens_out,unknown_tokens=unknown_tokens-(` + unknown("OLD") + `) WHERE ` + strings.Join(predicates, " AND ") + `; DELETE FROM ` + table + ` WHERE ` + strings.Join(predicates, " AND ") + ` AND observations=0;`
	for _, trigger := range []struct{ name, event, body string }{{"insert", "INSERT", add}, {"update", "UPDATE", remove + add}, {"delete", "DELETE", remove}} {
		if _, err = tx.ExecContext(ctx, "CREATE TRIGGER "+prefix+trigger.name+" AFTER "+trigger.event+" ON query_usage BEGIN "+trigger.body+" END"); err != nil {
			return err
		}
	}
	if err = checkSpace(); err != nil {
		return err
	}
	return tx.Commit()
}
