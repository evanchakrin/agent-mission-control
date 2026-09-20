package indexer

import (
	"context"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type SourceWork struct {
	SourceID   string `json:"sourceId"`
	Generation string `json:"generation"`
}

type SourceResult struct {
	Records int64  `json:"records"`
	Error   string `json:"error,omitempty"`
}

// GroupOnce prepares bounded source batches outside the writer transaction.
// Results are in request order, and positive record counts describe only durable
// commits. One bad source cannot discard progress for the other sources.
func (x *Indexer) GroupOnce(ctx context.Context, sources []SourceWork) ([]SourceResult, error) {
	return x.groupOnce(ctx, sources, x.Store.CommitIndexGroup)
}

func (x *Indexer) groupOnce(ctx context.Context, sources []SourceWork, commit func(context.Context, []store.IndexBatch) error) ([]SourceResult, error) {
	return x.publishGroup(ctx, sources, x.PrepareOnce, commit)
}

// PublishGroup keeps parsing outside hub writer admission. Preparation may run
// in a child; all publication and recovery commits use this indexer's Store.
func (x *Indexer) PublishGroup(ctx context.Context, sources []SourceWork, prepare PrepareFunc) ([]SourceResult, error) {
	return x.publishGroup(ctx, sources, prepare, x.Store.CommitIndexGroup)
}

func (x *Indexer) publishGroup(ctx context.Context, sources []SourceWork, prepare PrepareFunc, commit func(context.Context, []store.IndexBatch) error) ([]SourceResult, error) {
	if len(sources) == 0 || len(sources) > store.MaxIndexGroupSources {
		return nil, store.ErrInvalid
	}
	seen := make(map[string]bool, len(sources))
	for _, s := range sources {
		if s.SourceID == "" || s.Generation == "" || seen[s.SourceID] {
			return nil, store.ErrInvalid
		}
		seen[s.SourceID] = true
	}
	results := make([]SourceResult, len(sources))
	var batches []store.IndexBatch
	var positions []int
	var counts []int64
	var budget int64
	set := func(i int, n int64, err error) {
		if err != nil {
			results[i] = SourceResult{Error: err.Error()}
		} else {
			results[i] = SourceResult{Records: n}
		}
	}
	flush := func() {
		if len(batches) == 0 {
			return
		}
		err := commit(ctx, batches)
		for j, i := range positions {
			if err == nil {
				set(i, counts[j], nil)
			} else {
				// Re-read durable checkpoints after rollback or an uncertain
				// commit outcome. Never replay stale prepared contributions.
				n, e := x.PublishOnce(ctx, sources[i].SourceID, sources[i].Generation, prepare)
				set(i, n, e)
			}
		}
		batches, positions, counts = nil, nil, nil
		budget = 0
	}
	for i, s := range sources {
		prepared, err := prepare(ctx, s.SourceID, s.Generation)
		if err != nil {
			set(i, 0, err)
			continue
		}
		if prepared.Batch != nil {
			b := *prepared.Batch
			cost := store.IndexBatchBudget(b)
			// Keep retained preparation small; a single large record retains
			// the existing single-source path and parser memory limits.
			if cost > (4<<20)-budget {
				flush()
			}
			if cost > 4<<20 {
				n, e := x.publishPreparation(ctx, prepared)
				set(i, n, e)
				continue
			}
			batches = append(batches, b)
			positions = append(positions, i)
			budget += cost
			counts = append(counts, prepared.Records)
		} else {
			n, e := x.publishPreparation(ctx, prepared)
			set(i, n, e)
		}
	}
	flush()
	return results, nil
}
