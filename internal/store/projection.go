package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func pageLimit(n int) int {
	if n <= 0 {
		return 100
	}
	if n > 500 {
		return 500
	}
	return n
}

// CommitIndex publishes parser state, logical observations and the session
// projection in one transaction. Replays cannot advance from a stale cursor.
// An old raw generation remains queryable as evidence but cannot replace the
// active generation's session totals.
func (s *Store) CommitIndex(ctx context.Context, b IndexBatch) error {
	return s.commitIndex(ctx, b, nil)
}

// A supplied transaction is owned, budgeted and committed by CommitIndexGroup.
func (s *Store) commitIndex(ctx context.Context, b IndexBatch, shared *sql.Tx) (resultErr error) {
	if s.statsOnly {
		var state map[string]json.RawMessage
		if json.Unmarshal(b.ParserState, &state) == nil {
			delete(state, "title")
			b.ParserState, _ = json.Marshal(state)
		}
		b.Session.Title = ""
		for i := range b.Usage {
			b.Usage[i].Evidence = nil
		}
	}
	started := time.Now()
	phase := "validate"
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("index transaction %s: %w (transaction elapsed %s)", phase, resultErr, time.Since(started).Round(time.Millisecond))
		}
	}()
	if !validID(b.SourceID) || !validID(b.Generation) || !validID(b.Session.ID) || b.FromOffset < 0 || b.ToOffset < b.FromOffset {
		return ErrInvalid
	}
	if !json.Valid(validJSON(b.ParserState)) || len(b.ParserState) > 4*1024*1024 {
		return fmt.Errorf("%w: parser state", ErrInvalid)
	}
	if b.Session.TokensIn < 0 || b.Session.TokensCache < 0 || b.Session.TokensCacheWrite < 0 || b.Session.TokensOut < 0 {
		return fmt.Errorf("%w: negative tokens", ErrInvalid)
	}
	// Reserve a conservative bounded-batch allowance before expanding database
	// and FTS pages. Storage pressure pauses indexing without touching old views.
	var err error
	tx := shared
	if tx == nil {
		budget := indexBatchBudget(b)
		phase = "reserve storage"
		s.capacityMu.Lock()
		err = s.capacity(budget)
		if err == nil {
			s.pendingBytes += budget
		}
		s.capacityMu.Unlock()
		if err != nil {
			return err
		}
		defer func() { s.capacityMu.Lock(); s.pendingBytes -= budget; s.capacityMu.Unlock() }()
		phase = "writer admission"
		if err := s.writeMu.LockContext(ctx); err != nil {
			return err
		}
		defer s.writeMu.Unlock()
		phase = "begin transaction"
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	phase = "load checkpoint"
	var indexed, durable int64
	var active, machine, provider, native, activeRevision string
	err = tx.QueryRowContext(ctx, `SELECT s.indexed_offset,s.durable_offset,i.active_generation,i.machine_id,i.provider,i.native_id,COALESCE((SELECT revision FROM active_projection p WHERE p.source_id=s.source_id AND p.generation=s.generation),'') FROM sources s JOIN source_identity i USING(source_id) WHERE s.source_id=? AND s.generation=?`, b.SourceID, b.Generation).Scan(&indexed, &durable, &active, &machine, &provider, &native, &activeRevision)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	staging := false
	var revision ProjectionRevision
	if b.ProjectionRevision != "" {
		revision, err = scanRevision(tx.QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM projection_revisions WHERE revision=?", b.ProjectionRevision))
		if err != nil {
			return err
		}
		if revision.SourceID != b.SourceID || revision.Generation != b.Generation {
			return ErrConflict
		}
		switch revision.State {
		case "building":
			staging = true
			indexed = revision.IndexedOffset
			durable = revision.TargetOffset
		case "active":
			if activeRevision != b.ProjectionRevision {
				return ErrConflict
			}
		default:
			return fmt.Errorf("%w: projection is not writable", ErrConflict)
		}
		var checkpoint struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(b.ParserState, &checkpoint) != nil || checkpoint.Version != revision.ParserVersion {
			return fmt.Errorf("%w: parser revision version", ErrInvalid)
		}
	} else if activeRevision != "" {
		return fmt.Errorf("%w: active parser revision changed", ErrConflict)
	}
	if indexed != b.FromOffset || b.ToOffset > durable {
		return fmt.Errorf("%w: index bounds (indexed %d, durable %d)", ErrConflict, indexed, durable)
	}
	if b.Session.MachineID != "" && b.Session.MachineID != machine {
		return fmt.Errorf("%w: session machine", ErrInvalid)
	}
	b.Session.SourceID = b.SourceID
	b.Session.Generation = b.Generation
	b.Session.ProjectionRevision = b.ProjectionRevision
	b.Session.MachineID = machine
	b.Session.Provider = provider
	if b.Session.NativeID == "" {
		b.Session.NativeID = native
	}
	// A single projection ID cannot be used to replace a different source. Parent
	// and child threads are separate sources and must aggregate through queries.
	var priorSource string
	var priorJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT source_id,projection FROM sessions WHERE id=?`, b.Session.ID).Scan(&priorSource, &priorJSON)
	if err == nil && priorSource != b.SourceID {
		return fmt.Errorf("%w: session source", ErrConflict)
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var prior Session
	if len(priorJSON) > 0 {
		if err = json.Unmarshal(priorJSON, &prior); err != nil {
			return err
		}
	}

	if b.ProjectionRevision != "" {
		prior = revision.Session
	}
	if prior.Generation != b.Generation || prior.ProjectionRevision != b.ProjectionRevision {
		prior = Session{}
	}
	if b.Session.LastActivity.Before(prior.LastActivity) {
		b.Session.LastActivity = prior.LastActivity
	}
	if b.Session.Title == "" {
		b.Session.Title = prior.Title
	}
	if b.Session.Project == "" {
		b.Session.Project = prior.Project
	}
	if b.Session.ParentThreadID == "" {
		b.Session.ParentThreadID = prior.ParentThreadID
	}
	if b.Session.ForkedFromID == "" {
		b.Session.ForkedFromID = prior.ForkedFromID
	}
	if prior.Generation != b.Generation {
		prior = Session{}
	}
	b.Session.EventCount = prior.EventCount
	b.Session.TokensIn = prior.TokensIn
	b.Session.TokensCache = prior.TokensCache
	b.Session.TokensCacheWrite = prior.TokensCacheWrite
	b.Session.TokensOut = prior.TokensOut
	var hookEvidence hookIndexEvidence
	var undoEvidence undoIndexEvidence
	var fileEdits fileEditIndex
	if !s.statsOnly {
		hookEvidence, err = loadHookEvidence(ctx, tx, b)
		if err != nil {
			return err
		}
		undoEvidence, err = loadUndoEvidence(ctx, tx, b)
		if err != nil {
			return err
		}
		fileEdits, err = loadFileEdits(ctx, tx, b)
		if err != nil {
			return err
		}
	}
	for _, e := range b.Events {
		e.ID = revisionID(b.ProjectionRevision, e.ID)
		if s.statsOnly {
			e.Text, e.SearchText = "", ""
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(e.Data, &raw)
			kept := make(map[string]json.RawMessage)
			for _, key := range []string{"tool", "error", "code"} {
				if value, ok := raw[key]; ok {
					kept[key] = value
				}
			}
			e.Data, _ = json.Marshal(kept)
		}
		phase = "event"
		if !validID(e.ID) || e.SourceOffset < 0 || e.SourceLength < 0 || e.SourceOffset > durable || e.SourceLength > durable-e.SourceOffset || len(e.Text) > 8192 || len(e.Data) > 16384 || !json.Valid(validJSON(e.Data)) {
			return fmt.Errorf("%w: event bounds/preview", ErrInvalid)
		}
		if e.SessionID != "" && e.SessionID != b.Session.ID {
			return fmt.Errorf("%w: event session", ErrInvalid)
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, b.Session.ID, b.SourceID, b.Generation, e.AgentID, e.Kind, stamp(e.Timestamp), e.SourceOffset, e.SourceLength, e.Text, validJSON(e.Data), e.DedupeKey, b.ProjectionRevision)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			b.Session.EventCount += n
			seq, err := res.LastInsertId()
			if err != nil {
				return err
			}
			search := e.SearchText
			if search == "" {
				search = e.Text
			}
			if !s.statsOnly {
				hookEvidence.observe(e, search)
				if err = saveDelegationTask(ctx, tx, e, seq); err != nil {
					return err
				}
				if err = fileEdits.observe(ctx, tx, b, e, seq); err != nil {
					return err
				}
				if err = undoEvidence.observe(ctx, tx, b, e, seq); err != nil {
					return err
				}
				phase = "search text"
				if _, err = tx.ExecContext(ctx, `INSERT INTO events_fts(rowid,text) VALUES(?,?)`, seq, search); err != nil {
					return err
				}
			}
		} else {
			// Native deduplication may legitimately suppress a second content block,
			// but a colliding globally unique event ID must never cross sources.
			var source, generation string
			err = tx.QueryRowContext(ctx, `SELECT source_id,generation FROM events WHERE id=?`, e.ID).Scan(&source, &generation)
			if err == nil && (source != b.SourceID || generation != b.Generation) {
				return fmt.Errorf("%w: event identity", ErrConflict)
			}
			if err != nil && err != sql.ErrNoRows {
				return err
			}
		}
	}
	for _, u := range b.Usage {
		phase = "usage"
		u.ID = revisionID(b.ProjectionRevision, u.ID)
		if !validID(u.ID) || u.TokensIn < 0 || u.TokensCache < 0 || u.TokensCacheWrite < 0 || u.TokensOut < 0 || !json.Valid(validJSON(u.Evidence)) || len(u.Evidence) > 16384 {
			return fmt.Errorf("%w: usage observation", ErrInvalid)
		}
		if u.SessionID != "" && u.SessionID != b.Session.ID {
			return fmt.Errorf("%w: usage session", ErrInvalid)
		}
		u.SessionID = b.Session.ID
		var source, generation string
		var oldJSON []byte
		err = tx.QueryRowContext(ctx, `SELECT source_id,generation,observation FROM usage_observations WHERE id=?`, u.ID).Scan(&source, &generation, &oldJSON)
		if err == nil && (source != b.SourceID || generation != b.Generation) {
			return fmt.Errorf("%w: usage identity", ErrConflict)
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		var old UsageObservation
		if len(oldJSON) > 0 {
			if err = json.Unmarshal(oldJSON, &old); err != nil {
				return err
			}
		}
		u, err = mergeUsage(old, u)
		if err != nil {
			return err
		}
		data, err := json.Marshal(u)
		if err != nil {
			return err
		}
		// Materialized totals change by the revision delta, never a full history
		// scan and never the caller's guessed aggregate. A revised final message
		// can both increase and decrease the provider's earlier observation.
		for _, v := range []struct {
			total     *int64
			next, old int64
		}{
			{&b.Session.TokensIn, u.TokensIn, old.TokensIn}, {&b.Session.TokensCache, u.TokensCache, old.TokensCache},
			{&b.Session.TokensCacheWrite, u.TokensCacheWrite, old.TokensCacheWrite}, {&b.Session.TokensOut, u.TokensOut, old.TokensOut},
		} {
			if *v.total < v.old || v.next > (1<<63-1)-(*v.total-v.old) {
				return fmt.Errorf("%w: usage aggregate overflow or invalid revision", ErrInvalid)
			}
			*v.total += v.next - v.old
		}
		// A provider may revise a reply's usage on later content blocks. Retain the
		// latest complete observation at its native ID, never add the revision.
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_observations(id,session_id,source_id,generation,observation,projection_revision) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET observation=excluded.observation`, u.ID, b.Session.ID, b.SourceID, b.Generation, data, b.ProjectionRevision)
		if err != nil {
			return err
		}
	}

	if !s.statsOnly {
		if err = hookEvidence.save(ctx, tx, b); err != nil {
			return err
		}
		if err = undoEvidence.save(ctx, tx, b); err != nil {
			return err
		}
		if err = fileEdits.save(ctx, tx, b); err != nil {
			return err
		}
	}
	phase = "save projection"
	if s.statsOnly {
		b.Session.Title = ""
	}
	b.Session.Metadata = Metadata{}
	// New indexed evidence invalidates a checkpoint-bound price. Immutable
	// estimates remain in accounting history and can be compared independently.
	b.Session.Pricing = nil
	if b.ProjectionRevision != "" {
		data, e := json.Marshal(b.Session)
		if e != nil {
			return e
		}
		_, err = tx.ExecContext(ctx, `UPDATE projection_revisions SET indexed_offset=?,target_offset=CASE WHEN state='active' THEN MAX(target_offset,?) ELSE target_offset END,parser_state=?,projection=?,error='',updated_at=? WHERE revision=?`, b.ToOffset, durable, validJSON(b.ParserState), data, stamp(time.Now()), b.ProjectionRevision)
		if err != nil {
			return err
		}
	}
	if !staging && active == b.Generation {
		b.Session.Metadata = Metadata{}
		data, err := json.Marshal(b.Session)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET generation=excluded.generation,machine_id=excluded.machine_id,provider=excluded.provider,project=excluded.project,last_activity=excluded.last_activity,projection=excluded.projection`, b.Session.ID, b.SourceID, b.Generation, machine, provider, b.Session.Project, stamp(b.Session.LastActivity), data)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('session',?,?)`, b.Session.ID, stamp(time.Now())); err != nil {
			return err
		}
	}
	if staging {
		phase = "commit staged checkpoint"
		if shared != nil {
			return nil
		}
		return tx.Commit()
	}
	phase = "save checkpoint"
	_, err = tx.ExecContext(ctx, `UPDATE sources SET indexed_offset=?,parser_state=? WHERE source_id=? AND generation=?`, b.ToOffset, validJSON(b.ParserState), b.SourceID, b.Generation)
	if err != nil {
		return err
	}
	phase = "commit checkpoint"
	if shared != nil {
		return nil
	}
	return tx.Commit()
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	var b []byte
	var meta sql.NullString
	var machineName string
	err := s.db.QueryRowContext(ctx, `SELECT `+sessionProjectionJSON+`,m.value,COALESCE(NULLIF(ml.display_name,''),NULLIF(json_extract(mh.heartbeat,'$.name'),''),s.machine_id) FROM sessions s LEFT JOIN session_metadata m ON m.session_id=s.id LEFT JOIN machine_labels ml ON ml.machine_id=s.machine_id LEFT JOIN machines mh ON mh.machine_id=s.machine_id WHERE s.id=?`, id).Scan(&b, &meta, &machineName)
	if err == sql.ErrNoRows {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	if err = json.Unmarshal(b, &sess); err != nil {
		return sess, err
	}
	sess.MachineName = machineName
	if meta.Valid {
		err = json.Unmarshal([]byte(meta.String), &sess.Metadata)
	}
	return sess, err
}

func (s *Store) SessionIDForSource(ctx context.Context, sourceID string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE source_id=? ORDER BY id LIMIT 2`, sourceID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	id := ""
	for rows.Next() {
		if id != "" {
			return "", fmt.Errorf("%w: source has ambiguous session identities", ErrConflict)
		}
		if err = rows.Scan(&id); err != nil {
			return "", err
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if id == "" {
		return "", ErrNotFound
	}
	return id, nil
}

type sessionCursor struct {
	At string `json:"a"`
	ID string `json:"i"`
}

func sessionWhere(q SessionQuery, withCursor bool) (string, []any, error) {
	where := []string{"1=1"}
	args := []any{}
	if q.MachineID != "" {
		where = append(where, "s.machine_id=?")
		args = append(args, q.MachineID)
	}
	if q.Provider != "" {
		where = append(where, "s.provider=?")
		args = append(args, q.Provider)
	}
	if q.Project != "" {
		where = append(where, "COALESCE(NULLIF(json_extract(m.value,'$.project'),''),s.project)=?")
		args = append(args, q.Project)
	}
	if q.Archived != nil {
		where = append(where, "COALESCE(json_extract(m.value,'$.archived'),0)=?")
		args = append(args, *q.Archived)
	}
	if withCursor && q.Cursor != "" {
		var c sessionCursor
		b, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(b, &c) != nil || c.At == "" || c.ID == "" {
			return "", nil, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		where = append(where, "(s.last_activity<? OR (s.last_activity=? AND s.id>?))")
		args = append(args, c.At, c.At, c.ID)
	}
	return strings.Join(where, " AND "), args, nil
}

func (s *Store) ListSessions(ctx context.Context, q SessionQuery) (SessionPage, error) {
	return s.QuerySessions(ctx, q)
}

func (s *Store) SessionTotals(ctx context.Context, q SessionQuery) (Totals, error) {
	value, err := s.CatalogTotals(ctx, q)
	result := Totals{PricingTotals: value.PricingTotals, Sessions: value.Sessions, Events: value.Events, TokensIn: value.TokensIn, TokensCache: value.TokensCache, TokensCacheWrite: value.TokensCacheWrite, TokensOut: value.TokensOut, SessionsWithEstimate: value.SessionsWithEstimate, SessionsWithoutEstimate: value.SessionsWithoutEstimate, PricingCoverage: value.PricingCoverage}
	result.CurrentComparisons = value.CurrentComparisons
	if value.CostEstimate != nil {
		result.CostEstimate = *value.CostEstimate
	}
	return result, err
}

func (s *Store) ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) (EventPage, error) {
	return s.queryEvents(ctx, `e.session_id=? AND e.seq>?`, []any{sessionID, afterSeq}, limit, false)
}
func (s *Store) Search(ctx context.Context, q SearchQuery) (EventPage, error) {
	if q.Related && (!q.DelegatedOnly || q.AfterSequence != 0) {
		return EventPage{}, ErrInvalid
	}
	if strings.TrimSpace(q.Text) == "" || len(q.Text) > 4096 {
		return EventPage{}, ErrInvalid
	}
	terms := strings.Fields(q.Text)
	for i, t := range terms {
		terms[i] = "\"" + strings.ReplaceAll(t, "\"", "\"\"") + "\""
	}
	where := `events_fts MATCH ? AND events_fts.rowid>?`
	join := " AND "
	if q.Related {
		join = " OR "
		q.Limit = min(5, pageLimit(q.Limit))
	}
	args := []any{strings.Join(terms, join), q.AfterSequence}
	if q.DelegatedOnly {
		if err := s.ensureDelegatedIndex(ctx); err != nil {
			return EventPage{}, err
		}
		where += ` AND e.kind='tool-call' AND json_extract(e.data,'$.tool') IN ('Task','Agent','spawn_agent','functions.spawn_agent','collaboration.spawn_agent')`
	}
	if q.SessionID != "" {
		where += ` AND e.session_id=?`
		args = append(args, q.SessionID)
	}
	return s.queryEvents(ctx, where, args, q.Limit, true, q.Related, q.DelegatedOnly)
}
func (s *Store) queryEvents(ctx context.Context, where string, args []any, limit int, fts bool, ranked ...bool) (EventPage, error) {
	page := EventPage{Events: []Event{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	limit = pageLimit(limit)
	args = append(args, limit+1)
	// Only active source generations contribute to normal history queries.
	relevance := fts && len(ranked) > 0 && ranked[0]
	query := eventQuerySQL(where, fts, ranked...)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var ts string
		var data []byte
		var source, generation string
		values := []any{&e.Sequence, &e.ID, &e.SessionID, &e.AgentID, &e.Kind, &ts, &e.SourceOffset, &e.SourceLength, &e.Text, &data, &e.DedupeKey, &e.ProjectionRevision, &source, &generation, &e.AgentDisplayName}
		if fts {
			e.SearchContext = &EventSearchContext{}
			c := e.SearchContext
			values = append(values, &c.SessionTitle, &c.MachineID, &c.MachineName, &c.Provider, &c.Archived)
		}
		if err = rows.Scan(values...); err != nil {
			return page, err
		}
		e.Data = data
		if fts {
			e.HistorySnapshot = s.eventSnapshot(e.SessionID, source, generation, e.ProjectionRevision)
		}
		e.Timestamp, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return page, err
		}
		page.Events = append(page.Events, e)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		if !relevance {
			page.NextSequence = page.Events[limit-1].Sequence
		}
	}
	return page, nil
}

func eventQuerySQL(where string, fts bool, ranked ...bool) string {
	contextColumns := ""
	active := `i.id=e.session_id AND i.source_id=e.source_id AND i.generation=e.generation AND i.projection_revision=e.projection_revision`
	from, order := `events e JOIN query_sessions i ON `+active, `e.seq`
	if !fts && where == toolCallWhere {
		// The exact tool-call page predicate admits this partial index. Reading
		// call pages must not walk every unrelated event in a large transcript.
		from = `events e INDEXED BY events_tool_call_page JOIN query_sessions i ON ` + active
	}
	if fts {
		// FTS can stream matching rowids in order. Force it to drive the joins
		// and order by its rowid, otherwise SQLite sorts every matching event
		// (including large bodies) before applying the small result-page limit.
		from = `events_fts CROSS JOIN events e ON e.seq=events_fts.rowid CROSS JOIN query_sessions i ON ` + active
		order = `events_fts.rowid`
		if len(ranked) > 0 && ranked[0] {
			// FTS5 supplies BM25 rank order without a Go corpus-sized sort.
			// Equal ranks may tie; this is top-k retrieval, not a cursor order.
			order = `events_fts.rank`
		}
		contextColumns = `,i.title,i.machine_id,COALESCE(NULLIF((SELECT display_name FROM machine_labels ml WHERE ml.machine_id=i.machine_id),''),(SELECT json_extract(m.heartbeat,'$.name') FROM machines m WHERE m.machine_id=i.machine_id),''),i.provider,i.archived`
		if len(ranked) > 1 && ranked[1] {
			from = `events e INDEXED BY events_delegated_page CROSS JOIN events_fts ON events_fts.rowid=e.seq CROSS JOIN query_sessions i ON ` + active
			order = `e.seq`
			if ranked[0] {
				order = `bm25(events_fts)`
			}
		}
	}
	return `SELECT e.seq,e.id,e.session_id,e.agent_id,e.kind,e.timestamp,e.source_offset,e.raw_length,e.text,e.data,e.dedupe_key,e.projection_revision,e.source_id,e.generation,COALESCE((SELECT name FROM agent_names n WHERE n.session_id=e.session_id AND n.agent_id=e.agent_id),'')` + contextColumns + ` FROM ` + from + ` WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?`
}

func (s *Store) GetUsage(ctx context.Context, sessionID, afterID string, limit int) ([]UsageObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.observation FROM usage_observations u JOIN sessions i ON i.id=u.session_id AND i.source_id=u.source_id AND i.generation=u.generation AND COALESCE(json_extract(i.projection,'$.projectionRevision'),'')=u.projection_revision WHERE u.session_id=? AND u.id>? ORDER BY u.id LIMIT ?`, sessionID, afterID, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageObservation{}
	for rows.Next() {
		var u UsageObservation
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ChangeHead reads the rightmost primary-key entry, without replaying history.
// It is an invalidation hint, not a replacement for the resumable change ledger.
func (s *Store) ChangeHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM changes`).Scan(&head)
	return head, err
}

func (s *Store) Changes(ctx context.Context, after int64, limit int) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,kind,session_id,at FROM changes WHERE seq>? ORDER BY seq LIMIT ?`, after, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		var at string
		if err = rows.Scan(&c.Sequence, &c.Kind, &c.SessionID, &at); err != nil {
			return nil, err
		}
		c.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListMachines(ctx context.Context) ([]Machine, error) { return s.Machines(ctx) }

// SourceCursor returns the opaque inventory cursor after an item.
func SourceCursor(st SourceState) string {
	b, _ := json.Marshal([]string{st.Source.SourceID, st.Source.Generation})
	return base64.RawURLEncoding.EncodeToString(b)
}
func (s *Store) ListSources(ctx context.Context, after string, limit int) ([]SourceState, error) {
	id, gen := "", ""
	if after != "" {
		b, err := base64.RawURLEncoding.DecodeString(after)
		var c []string
		if err != nil || json.Unmarshal(b, &c) != nil || len(c) != 2 {
			return nil, ErrInvalid
		}
		id, gen = c[0], c[1]
	}
	return s.SourcesAfter(ctx, id, gen, limit)
}
