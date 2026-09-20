package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Windows service installations must not depend on a user's zoneinfo files.
)

type CalendarQuery struct {
	SessionQuery
	Start    string
	End      string
	Timezone string
	Snapshot string
}

type CalendarDay struct {
	Day                  string    `json:"day"`
	From                 time.Time `json:"from"`
	To                   time.Time `json:"to"`
	Sessions             int64     `json:"sessions"`
	CostEstimate         *float64  `json:"costEstimate"`
	SessionsWithEstimate int64     `json:"sessionsWithEstimate"`
	RecordedTokens       int64     `json:"recordedTokens"`
	PricedTokens         int64     `json:"pricedTokens"`
	Errors               int64     `json:"errors"`
	UnknownToolResults   int64     `json:"unknownToolResults"`
	AgentScopes          int64     `json:"agentScopes"`
	TopTierCost          *float64  `json:"topTierCost"`
	TierPricedTokens     int64     `json:"tierPricedTokens"`
}

type CalendarPage struct {
	Snapshot   string        `json:"snapshot"`
	Days       []CalendarDay `json:"days"`
	Timezone   string        `json:"timezone"`
	Basis      string        `json:"basis"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// Find the first instant of a civil date. Some zones change clocks at midnight,
// so time.Date(..., 00:00, zone) can normalize into the preceding date. Searching
// integer seconds also chooses the first midnight when it occurs twice.
func calendarBoundary(date time.Time, zone *time.Location) (time.Time, error) {
	key := date.Format("2006-01-02")
	low, high := date.Add(-48*time.Hour).Unix(), date.Add(48*time.Hour).Unix()
	for low < high {
		middle := low + (high-low)/2
		if time.Unix(middle, 0).In(zone).Format("2006-01-02") < key {
			low = middle + 1
		} else {
			high = middle
		}
	}
	boundary := time.Unix(low, 0).UTC()
	if boundary.In(zone).Format("2006-01-02") != key {
		return time.Time{}, fmt.Errorf("%w: calendar date does not exist in timezone", ErrInvalid)
	}
	return boundary, nil
}

// Calendar groups each session once by its latest recorded activity, not by
// usage-observation days. Day boundaries are civil midnights, including DST.
// Session, event and usage read models are joined only at their active revision.
func (s *Store) Calendar(ctx context.Context, q CalendarQuery) (CalendarPage, error) {
	page := CalendarPage{Days: []CalendarDay{}, Timezone: q.Timezone, Basis: "session-last-activity; undated sessions excluded; agent scopes distinct within each session; tiers from selected immutable rate catalogs"}
	if q.Timezone == "" || q.Timezone == "Local" || len(q.Timezone) > 128 || q.From != nil || q.To != nil {
		return page, ErrInvalid
	}
	zone, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return page, ErrInvalid
	}
	start, err := time.Parse("2006-01-02", q.Start)
	if err != nil {
		return page, ErrInvalid
	}
	end, err := time.Parse("2006-01-02", q.End)
	if err != nil || !start.Before(end) {
		return page, ErrInvalid
	}
	// At most 400 civil days per calendar, with bounded 100-day result pages.
	if end.After(start.AddDate(0, 0, 400)) || q.Limit < 0 || q.Limit > 100 {
		return page, ErrInvalid
	}
	where, args, err := catalogWhere(q.SessionQuery)
	if err != nil {
		return page, err
	}
	if len(q.Snapshot) > 64 || q.Cursor != "" && q.Snapshot == "" {
		return page, ErrInvalid
	}
	if err = s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	if err = s.InitializeAccounting(ctx); err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var head int64
	var epoch string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0),(SELECT value FROM properties WHERE key='recovery_epoch') FROM changes`).Scan(&head, &epoch); err != nil {
		return page, err
	}
	page.Snapshot = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("calendar-v2:%s:%d", epoch, head))))
	if q.Snapshot != "" && q.Snapshot != page.Snapshot {
		return page, ErrHistoryChanged
	}
	identity := q
	identity.Snapshot = page.Snapshot
	identity.Cursor = ""
	identity.Limit = 0
	encoded, _ := json.Marshal(identity)
	dimension := fmt.Sprintf("calendar:%x", sha256.Sum256(encoded))
	after, has, err := decodeKey(q.Cursor, dimension)
	if err != nil {
		return page, err
	}
	if has {
		next, e := time.Parse("2006-01-02", after)
		if e != nil || next.Before(start) || !next.Before(end) {
			return page, ErrInvalid
		}
		start = next
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	values := []string{}
	boundArgs := []any{}
	for day := start; day.Before(end) && len(page.Days) < limit; {
		next := day.AddDate(0, 0, 1)
		if !next.After(day) {
			return page, ErrInvalid
		}
		from, e := calendarBoundary(day, zone)
		if e != nil {
			return page, e
		}
		to, e := calendarBoundary(next, zone)
		if e != nil {
			return page, e
		}
		page.Days = append(page.Days, CalendarDay{Day: day.Format("2006-01-02"), From: from, To: to})
		values = append(values, "(?,?,?)")
		boundArgs = append(boundArgs, day.Format("2006-01-02"), stamp(from), stamp(to))
		day = next
		if len(page.Days) == limit && day.Before(end) {
			page.NextCursor = encodeKey(day.Format("2006-01-02"), dimension)
		}
	}
	// Aggregate child rows per session before summing days: a session with many
	// agents or observations must not multiply its tokens or estimate.
	query := calendarSQL(values, where)
	rows, err := tx.QueryContext(ctx, query, append(boundArgs, args...)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		if i >= len(page.Days) {
			return page, ErrInvalid
		}
		d := &page.Days[i]
		var day string
		var cost sql.NullFloat64
		var topCost sql.NullFloat64
		if err = rows.Scan(&day, &d.Sessions, &cost, &d.SessionsWithEstimate, &d.RecordedTokens, &d.PricedTokens, &d.Errors, &d.UnknownToolResults, &d.AgentScopes, &topCost, &d.TierPricedTokens); err != nil {
			return page, err
		}
		if day != d.Day {
			return page, ErrInvalid
		}
		if cost.Valid {
			d.CostEstimate = &cost.Float64
		}
		if topCost.Valid {
			d.TopTierCost = &topCost.Float64
		}
		if d.TierPricedTokens < 0 || d.TierPricedTokens > d.PricedTokens {
			return page, ErrInvalid
		}
		i++
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if i != len(page.Days) {
		return page, ErrInvalid
	}
	return page, nil
}

func calendarSQL(values []string, where string) string {
	return `WITH days(day,from_at,to_at) AS (VALUES ` + strings.Join(values, ",") + `),
 selected AS MATERIALIZED (SELECT d.day,q.* FROM days d JOIN query_sessions q ON q.last_activity>=d.from_at AND q.last_activity<d.to_at WHERE ` + where + `),
 ` + selectedTierCTEs + `
 measured AS (SELECT q.*,
 (SELECT COALESCE(SUM(a.errors),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) AS errors,
 (SELECT COALESCE(SUM(a.unknown_results),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) AS unknown_results,
 ` + calendarAgentScopeCount + ` AS agents
 FROM selected q)
 SELECT d.day,COUNT(q.id),SUM(q.cost_estimate),COUNT(q.cost_estimate),
 COALESCE(SUM(q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out),0),COALESCE(SUM(q.priced_tokens),0),
 COALESCE(SUM(q.errors),0),COALESCE(SUM(q.unknown_results),0),COALESCE(SUM(q.agents),0),SUM(t.top_cost),COALESCE(SUM(t.tier_tokens),0)
 FROM days d LEFT JOIN measured q ON q.day=d.day LEFT JOIN tiers t ON t.session_id=q.id GROUP BY d.day ORDER BY d.day`
}
