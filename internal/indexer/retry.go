package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Service one due failure per interval without resetting or waiting for the
// normal catalog cursor. Normal ingestion and rebuilds still get their turns.
func (x *Indexer) retryIfDue(ctx context.Context, last *time.Time, now time.Time) (bool, error) {
	if now.Sub(*last) < 30*time.Second {
		return false, nil
	}
	sources, err := x.Store.RetrySources(ctx, 1)
	if err != nil {
		return false, err
	}
	*last = now
	if len(sources) == 0 {
		return false, nil
	}
	return x.processSource(ctx, sources[0])
}

func (x *Indexer) processSource(ctx context.Context, s store.SourceState) (bool, error) {
	if s.ExternalIndex || s.IndexedOffset >= s.DurableOffset {
		return false, nil
	}
	process := x.Process
	if process == nil {
		process = x.Once
	}
	n, problem := process(ctx, s.Source.SourceID, s.Source.Generation)
	return x.finishSource(ctx, s, n, problem)
}

func (x *Indexer) processSources(ctx context.Context, sources []store.SourceState) (bool, error) {
	progress := false
	for len(sources) > 0 {
		seen := make(map[string]bool)
		n := 0
		for n < len(sources) && n < store.MaxIndexGroupSources && !seen[sources[n].Source.SourceID] {
			seen[sources[n].Source.SourceID] = true
			n++
		}
		// Rewritten generations can be adjacent in the catalog. Preserve
		// ordering but never send two generations of one source in a group.
		advanced, err := x.processSourceGroup(ctx, sources[:n])
		if err != nil {
			return progress, err
		}
		progress = progress || advanced
		sources = sources[n:]
	}
	return progress, nil
}

func (x *Indexer) processSourceGroup(ctx context.Context, sources []store.SourceState) (bool, error) {
	work := make([]SourceWork, len(sources))
	for i, s := range sources {
		work[i] = SourceWork{SourceID: s.Source.SourceID, Generation: s.Source.Generation}
	}
	results, problem := x.ProcessGroup(ctx, work)
	if problem == nil && len(results) != len(sources) {
		problem = errors.New("parser returned an incomplete source group")
	}
	progress := false
	for i, s := range sources {
		var n int64
		e := problem
		if e == nil {
			n = results[i].Records
			if results[i].Error != "" {
				e = errors.New(results[i].Error)
			}
		}
		advanced, err := x.finishSource(ctx, s, n, e)
		if err != nil {
			return progress, err
		}
		progress = progress || advanced
	}
	return progress, nil
}

func (x *Indexer) finishSource(ctx context.Context, s store.SourceState, n int64, problem error) (bool, error) {
	advanced := n > 0
	if problem == nil && !advanced {
		advanced, problem = x.Store.PendingParserScan(ctx, s.Source.SourceID, s.Source.Generation)
	}
	// A committed record batch already removes completed work or clears retry
	// state atomically with its checkpoint. Avoid another writer acquisition on
	// every successful batch. Scan-only progress and failures still need recording.
	if n == 0 || problem != nil {
		if err := x.Store.RecordIndexAttempt(ctx, s.Source.SourceID, s.Source.Generation, s.DurableOffset, advanced, problem); err != nil {
			return false, err
		}
	}
	if problem != nil {
		x.note("blocked", problem, 0)
		return false, nil
	}
	if advanced {
		x.note("indexing", nil, n)
	}
	return advanced, nil
}
