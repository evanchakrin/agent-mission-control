package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type RingWeek struct {
	RhythmBucket
	Week string    `json:"week"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}
type RingsResult struct {
	Project         string     `json:"project"`
	Timezone        string     `json:"timezone"`
	Sessions        int64      `json:"sessions"`
	OlderSessions   int64      `json:"olderSessions"`
	UndatedSessions int64      `json:"undatedSessions"`
	Weeks           []RingWeek `json:"weeks"`
}

// One project's most recent ten civil weeks, anchored to its last recorded
// activity (not today's date). Session/usage bodies are never loaded into Go.
func (s *Store) Rings(ctx context.Context, q RhythmQuery) (RingsResult, error) {
	out := RingsResult{Project: q.Project, Timezone: q.Timezone, Weeks: []RingWeek{}}
	if q.Timezone == "" || q.Timezone == "Local" || len(q.Timezone) > 128 || q.Project == "" && !q.UnassignedProject || q.From != nil || q.To != nil || q.Cursor != "" || q.Limit != 0 {
		return out, ErrInvalid
	}
	zone, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return out, ErrInvalid
	}
	filters := q.SessionQuery
	no := false
	filters.Archived = &no
	where, args, err := catalogWhere(filters)
	if err != nil {
		return out, err
	}
	if err = s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	if err = s.InitializeAccounting(ctx); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var earliest, latest sql.NullString
	var dated int64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(CASE WHEN q.last_activity>'0001-01-01T00:00:00.000000000Z' THEN 1 END),MIN(CASE WHEN q.last_activity>'0001-01-01T00:00:00.000000000Z' THEN q.last_activity END),MAX(CASE WHEN q.last_activity>'0001-01-01T00:00:00.000000000Z' THEN q.last_activity END) FROM query_sessions q WHERE `+where, args...).Scan(&out.Sessions, &dated, &earliest, &latest)
	if err != nil {
		return out, err
	}
	out.UndatedSessions = out.Sessions - dated
	if dated == 0 {
		return out, nil
	}
	first, err := time.Parse(time.RFC3339Nano, earliest.String)
	if err != nil {
		return out, err
	}
	last, err := time.Parse(time.RFC3339Nano, latest.String)
	if err != nil {
		return out, err
	}
	monday := func(at time.Time) time.Time {
		local := at.In(zone)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		return day.AddDate(0, 0, -(int(local.Weekday())+6)%7)
	}
	anchor := monday(last)
	start := anchor.AddDate(0, 0, -63)
	if older := monday(first); older.After(start) {
		start = older
	}
	values := []string{}
	bounds := []any{}
	for day := start; !day.After(anchor); day = day.AddDate(0, 0, 7) {
		from, e := calendarBoundary(day, zone)
		if e != nil {
			return out, e
		}
		to, e := calendarBoundary(day.AddDate(0, 0, 7), zone)
		if e != nil {
			return out, e
		}
		out.Weeks = append(out.Weeks, RingWeek{RhythmBucket: RhythmBucket{Index: len(out.Weeks)}, Week: day.Format("2006-01-02"), From: from, To: to})
		values = append(values, "(?,?,?)")
		bounds = append(bounds, day.Format("2006-01-02"), stamp(from), stamp(to))
	}
	rows, err := tx.QueryContext(ctx, ringsSQL(values, where), append(bounds, args...)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var shown int64
	i := 0
	for rows.Next() {
		if i >= len(out.Weeks) {
			return out, ErrInvalid
		}
		w := &out.Weeks[i]
		var day string
		var cost, classified, top sql.NullFloat64
		if err = rows.Scan(&day, &w.Sessions, &w.SessionsWithErrors, &w.SessionsWithIncompleteResults, &w.RecordedTokens, &w.PricedTokens, &w.TierPricedTokens, &cost, &classified, &top); err != nil {
			return out, err
		}
		if day != w.Week {
			return out, ErrInvalid
		}
		if cost.Valid {
			w.CostEstimate = &cost.Float64
		}
		if classified.Valid {
			w.ClassifiedCost = &classified.Float64
		}
		if top.Valid {
			w.TopTierCost = &top.Float64
		}
		shown += w.Sessions
		i++
	}
	out.OlderSessions = dated - shown
	return out, rows.Err()
}

func ringsSQL(values []string, where string) string {
	return `WITH days(day,from_at,to_at) AS (VALUES ` + strings.Join(values, ",") + `),
 selected AS MATERIALIZED (SELECT d.day,q.* FROM days d JOIN query_sessions q ON q.last_activity>=d.from_at AND q.last_activity<d.to_at WHERE ` + where + `),` + selectedTierCTEs + `
 measured AS (SELECT q.*,
 (SELECT COALESCE(SUM(a.errors),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) failures,
 (SELECT COALESCE(SUM(a.unknown_results+a.indexing_errors),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) incomplete FROM selected q)
 SELECT d.day,COUNT(q.id),COALESCE(SUM(q.failures>0),0),COALESCE(SUM(q.incomplete>0),0),COALESCE(SUM(q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out),0),COALESCE(SUM(q.priced_tokens),0),COALESCE(SUM(t.tier_tokens),0),SUM(q.cost_estimate),SUM(t.known_cost),SUM(t.top_cost)
 FROM days d LEFT JOIN measured q ON q.day=d.day LEFT JOIN tiers t ON t.session_id=q.id GROUP BY d.day ORDER BY d.day`
}
