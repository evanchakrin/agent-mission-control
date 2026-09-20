// Package indexer advances durable parser checkpoints in bounded batches.
package indexer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type Status struct {
	State                string                 `json:"state"`
	Error                string                 `json:"error,omitempty"`
	LastProgress         time.Time              `json:"lastProgress"`
	Records              int64                  `json:"records"`
	Durable              store.IndexingProgress `json:"durable"`
	Rebuilds             store.RebuildProgress  `json:"rebuilds"`
	VersionCheckComplete bool                   `json:"versionCheckComplete"`
	VersionIssues        int64                  `json:"versionIssues"`
	VersionExample       *VersionIssue          `json:"versionExample,omitempty"`
}
type Indexer struct {
	Store          *store.Store
	Process        func(context.Context, string, string) (int64, error)
	ProcessGroup   func(context.Context, []SourceWork) ([]SourceResult, error)
	RebuildProcess func(context.Context, string) (int64, error)
	mu             sync.RWMutex
	status         Status
}

func (x *Indexer) Status() Status { x.mu.RLock(); defer x.mu.RUnlock(); return x.status }
func (x *Indexer) refreshIfDue(ctx context.Context, last *time.Time, now time.Time) error {
	if now.Sub(*last) < 30*time.Second {
		return nil
	}
	if err := x.refresh(ctx); err != nil {
		return err
	}
	*last = now
	return nil
}
func (x *Indexer) refresh(ctx context.Context) error {
	p, err := x.Store.IndexingProgress(ctx)
	if err != nil {
		return err
	}
	r, err := x.Store.RebuildProgress(ctx)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.status.Durable = p
	x.status.Rebuilds = r
	x.status.LastProgress = p.LastProgress
	x.status.Error = p.Problem
	switch {
	case r.Blocked > 0:
		x.status.State = "rebuild-blocked"
		x.status.Error = r.Problem
	case r.Pending > 0:
		x.status.State = "rebuilding"
	case x.status.VersionIssues > 0:
		x.status.State = "rebuild-required"
	case p.Blocked > 0:
		x.status.State = "blocked"
	case p.Ready > 0:
		x.status.State = "indexing"
	case p.AwaitingRecord > 0:
		x.status.State = "awaiting-record"
	default:
		x.status.State = "caught-up"
	}
	return nil
}
func (x *Indexer) note(state string, err error, records int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.status.State = state
	x.status.Error = ""
	if err != nil {
		x.status.Error = err.Error()
	}
	if records > 0 {
		x.status.LastProgress = time.Now()
		x.status.Records += records
	}
}

// Run is fair across the durable source catalog. No in-memory corpus catalog or
// eager transcript load is required. The heartbeat uses an independent handler.
func (x *Indexer) Run(ctx context.Context) error {
	for {
		err := x.run(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !store.IsContention(err) {
			return err
		}
		// SQLITE_BUSY/LOCKED is contention, not a fatal hub error. The failed
		// transaction rolled back; resume only from durable work/checkpoints.
		x.note("blocked_storage", err, 0)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (x *Indexer) run(ctx context.Context) error {
	if err := x.Store.SetupIndexing(ctx); err != nil {
		return err
	}
	x.mu.Lock()
	x.status.VersionIssues = 0
	x.status.VersionExample = nil
	x.status.VersionCheckComplete = false
	x.mu.Unlock()
	for cursor := ""; ; {
		p, err := x.CheckVersions(ctx, cursor, 250)
		if err != nil {
			return err
		}
		x.mu.Lock()
		x.status.VersionIssues += int64(len(p.Issues))
		if x.status.VersionExample == nil && len(p.Issues) > 0 {
			v := p.Issues[0]
			x.status.VersionExample = &v
		}
		x.status.VersionCheckComplete = p.Next == ""
		x.mu.Unlock()
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	if err := x.refresh(ctx); err != nil {
		return err
	}
	lastRefresh := time.Now()
	lastRetry := time.Now()
	cursor := ""
	for {
		progress, err := x.retryIfDue(ctx, &lastRetry, time.Now())
		if err != nil {
			return err
		}
		jobs, err := x.Store.RebuildWork(ctx, 1)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			prior, e := x.Store.ProjectionRevision(ctx, job)
			if e != nil {
				return e
			}
			before, e := x.Store.SourceState(ctx, prior.SourceID, prior.Generation)
			if e != nil {
				return e
			}
			process := x.RebuildProcess
			if process == nil {
				process = x.RebuildOnce
			}
			n, e := process(ctx, job)
			if e2 := x.Store.RebuildProblem(ctx, job, e); e2 != nil {
				return e2
			}
			if e != nil {
				x.note("rebuild-blocked", e, 0)
			} else {
				// Verification checkpoints are durable work even when no new
				// transcript records were parsed in this dispatch.
				progress = true
				if n > 0 {
					progress = true
					x.note("rebuilding", nil, n)
				}
				after, err := x.Store.ProjectionRevision(ctx, job)
				if err != nil {
					return err
				}
				if after.State == "active" && prior.State != "active" {
					x.rebuiltVersion(before)
					if err = x.refresh(ctx); err != nil {
						return err
					}
					lastRefresh = time.Now()
				}
			}
		}
		{
			// Alternate one bounded ingestion dispatch with one rebuild
			// dispatch. Preserve the cursor across turns so neither rebuilds
			// nor later sources wait behind a whole catch-up catalog pass.
			limit := 1
			if x.ProcessGroup != nil {
				limit = store.MaxIndexGroupSources
			}
			sources, err := x.Store.PendingSources(ctx, cursor, limit)
			if err != nil {
				return err
			}
			if len(sources) > 0 && x.ProcessGroup != nil {
				advanced, err := x.processSources(ctx, sources)
				if err != nil {
					return err
				}
				progress = progress || advanced
				cursor = store.SourceCursor(sources[len(sources)-1])
				if err := x.refreshIfDue(ctx, &lastRefresh, time.Now()); err != nil {
					return err
				}
			} else {
				for _, s := range sources {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					advanced, err := x.processSource(ctx, s)
					if err != nil {
						return err
					}
					progress = progress || advanced
					cursor = store.SourceCursor(s)
					// Catch-up can take hours for a catalog. Refresh durable health
					// between bounded dispatches, not only after the entire catalog.
					if err := x.refreshIfDue(ctx, &lastRefresh, time.Now()); err != nil {
						return err
					}
				}
			}
			if len(sources) == 0 {
				// Wrap without the idle delay when earlier sources may still
				// have work. An empty catalog on the next pass sleeps normally.
				progress = progress || cursor != ""
				cursor = ""
			}
		}
		if err := x.refreshIfDue(ctx, &lastRefresh, time.Now()); err != nil {
			return err
		}
		if !progress {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (x *Indexer) Once(ctx context.Context, sourceID, generation string) (int64, error) {
	return x.onceWithCommit(ctx, sourceID, generation, x.Store.CommitIndex)
}

func (x *Indexer) onceWithCommit(ctx context.Context, sourceID, generation string, commit func(context.Context, store.IndexBatch) error) (int64, error) {
	return x.onceWithSinks(ctx, sourceID, generation, commit, x.Store.SaveParserScan)
}

func (x *Indexer) onceWithSinks(ctx context.Context, sourceID, generation string, commit func(context.Context, store.IndexBatch) error, scan func(context.Context, store.SourceState, json.RawMessage) error) (int64, error) {
	src, err := x.Store.SourceState(ctx, sourceID, generation)
	if err != nil {
		return 0, err
	}
	if src.ExternalIndex {
		return 0, nil
	}
	id, err := x.Store.SessionIDForSource(ctx, sourceID)
	if errors.Is(err, store.ErrNotFound) {
		id = parser.SessionID(src.Source)
	} else if err != nil {
		return 0, err
	}
	return x.indexSourceWithSinks(ctx, src, id, commit, scan)
}

func (x *Indexer) indexSource(ctx context.Context, src store.SourceState, sessionID string) (int64, error) {
	return x.indexSourceWithCommit(ctx, src, sessionID, x.Store.CommitIndex)
}

func (x *Indexer) indexSourceWithCommit(ctx context.Context, src store.SourceState, sessionID string, commit func(context.Context, store.IndexBatch) error) (int64, error) {
	return x.indexSourceWithSinks(ctx, src, sessionID, commit, x.Store.SaveParserScan)
}

func (x *Indexer) indexSourceWithSinks(ctx context.Context, src store.SourceState, sessionID string, commit func(context.Context, store.IndexBatch) error, scan func(context.Context, store.SourceState, json.RawMessage) error) (int64, error) {
	started := time.Now()
	sourceID, generation := src.Source.SourceID, src.Source.Generation
	state, err := parser.DecodeState(src.ParserState)
	if err != nil {
		return 0, err
	}
	scanOffset := state.ScanOffset
	if scanOffset == 0 {
		scanOffset = src.IndexedOffset
	}
	if scanOffset < src.IndexedOffset || scanOffset > src.DurableOffset {
		return 0, fmt.Errorf("damaged parser scan checkpoint")
	}
	raw, err := x.Store.OpenSource(ctx, sourceID, generation, scanOffset)
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	scanBudget := min(int64(4<<20), src.DurableOffset-scanOffset)
	r := bufio.NewReaderSize(io.LimitReader(raw, scanBudget), 64*1024)
	scanStart := scanOffset
	batch := store.IndexBatch{SourceID: sourceID, Generation: generation, ProjectionRevision: src.ProjectionRevision, FromOffset: src.IndexedOffset, ToOffset: src.IndexedOffset}
	batch.Session = store.Session{ID: sessionID, MachineID: src.Source.MachineID, Provider: src.Source.Provider, NativeID: state.NativeID, Completeness: "indexed-source"}
	offset := src.IndexedOffset
	var records int64
	// A resumed record may include bytes scanned in earlier dispatches. Count
	// those bytes against this transaction too; finish that record, then commit
	// it alone instead of accumulating another batch behind it.
	for records < 256 && scanOffset-scanStart < scanBudget && offset-src.IndexedOffset < 4<<20 {
		if ctx.Err() != nil {
			return records, ctx.Err()
		}
		var line []byte
		length := scanOffset - offset
		resuming := length > 0
		tooLarge := length > parser.MaxRecordBytes
		for {
			if ctx.Err() != nil {
				return records, ctx.Err()
			}
			part, readErr := r.ReadSlice('\n')
			length += int64(len(part))
			scanOffset += int64(len(part))
			if !tooLarge {
				if length > parser.MaxRecordBytes {
					tooLarge = true
					line = nil
				} else if !resuming {
					line = append(line, part...)
				}
			}
			if readErr == bufio.ErrBufferFull {
				continue
			}
			// An incomplete trailing record stays before the checkpoint, even if large.
			if errors.Is(readErr, io.EOF) {
				length = 0
				break
			}
			if readErr != nil {
				return records, fmt.Errorf("read durable source: %w", readErr)
			}
			break
		}
		if length == 0 {
			break
		}
		if resuming && !tooLarge {
			// At most one bounded record is reread after its newline is found;
			// partial prefixes are never reread on every append or dispatch.
			original, e := x.Store.OpenSource(ctx, sourceID, generation, offset)
			if e != nil {
				return records, e
			}
			line = make([]byte, int(length))
			_, e = io.ReadFull(original, line)
			closeErr := original.Close()
			if e != nil {
				return records, e
			}
			if closeErr != nil {
				return records, closeErr
			}
		}
		if tooLarge {
			batch.Events = append(batch.Events, store.Event{ID: parser.ID(sourceID, generation, fmt.Sprint(offset), "oversize"), SessionID: batch.Session.ID, AgentID: "main", Kind: "indexing-error", SourceOffset: offset, SourceLength: length, Text: "Record exceeds parser memory budget; raw download remains available", Data: json.RawMessage(`{"code":"record_too_large"}`)})
		} else {
			result := parser.Record(src.Source, &state, offset, line)
			for i := range result.Events {
				result.Events[i].SessionID = batch.Session.ID
			}
			for i := range result.Usage {
				result.Usage[i].SessionID = batch.Session.ID
			}
			batch.Events = append(batch.Events, result.Events...)
			batch.Usage = append(batch.Usage, result.Usage...)
			for _, e := range result.Events {
				if e.Timestamp.After(batch.Session.LastActivity) {
					batch.Session.LastActivity = e.Timestamp
				}
			}
		}
		offset += length
		records++
	}
	state.ScanOffset = 0
	if scanOffset > offset {
		state.ScanOffset = scanOffset
	}
	if records == 0 {
		if scanOffset > scanStart {
			checkpoint, e := json.Marshal(state)
			if e != nil {
				return 0, e
			}
			return 0, scan(ctx, src, checkpoint)
		}
		return 0, nil
	}
	batch.ToOffset = offset
	batch.Session.Title = state.Title
	batch.Session.Project = state.Project
	batch.Session.NativeID = state.NativeID
	batch.Session.ParentThreadID = state.ParentThreadID
	batch.Session.ForkedFromID = state.ForkedFromID
	if state.ClaudeIdentityScope == "agent" {
		batch.Session.NativeAgentID = state.ClaudeAgentID
	}
	batch.ParserState, err = json.Marshal(state)
	if err != nil {
		return 0, err
	}
	parseElapsed := time.Since(started).Round(time.Millisecond)
	if err = commit(ctx, batch); err != nil {
		return 0, fmt.Errorf("commit parser batch (%d records, %d source bytes, preparation %s): %w", records, batch.ToOffset-batch.FromOffset, parseElapsed, err)
	}
	return records, nil
}
