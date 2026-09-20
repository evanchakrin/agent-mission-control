package indexer

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Preparation is not durable progress. A caller must commit the batch or scan
// checkpoint before reporting success, and discard/reprepare after conflicts.
// Exactly one of Batch and Scan can be present; neither means no new work.
type Preparation struct {
	Records int64
	Batch   *store.IndexBatch
	Scan    *ScanPreparation
}

type ScanPreparation struct {
	Source     store.SourceState
	Checkpoint json.RawMessage
}

type PrepareFunc func(context.Context, string, string) (Preparation, error)

// PublishOnce reports records only after the hub's durable commit succeeds.
func (x *Indexer) PublishOnce(ctx context.Context, sourceID, generation string, prepare PrepareFunc) (int64, error) {
	p, err := prepare(ctx, sourceID, generation)
	if err != nil {
		return 0, err
	}
	return x.publishPreparation(ctx, p)
}

func (x *Indexer) publishPreparation(ctx context.Context, p Preparation) (int64, error) {
	var err error
	if p.Batch != nil {
		err = x.Store.CommitIndex(ctx, *p.Batch)
	} else if p.Scan != nil {
		err = x.Store.SaveParserScan(ctx, p.Scan.Source, p.Scan.Checkpoint)
	}
	if err != nil {
		return 0, err
	}
	return p.Records, nil
}

// Public Event JSON intentionally excludes transient SearchText. Private IPC
// must preserve it or publishing a prepared batch would silently lose search
// coverage. Keep that evidence separate from the public event representation.
type preparationJSON Preparation
type preparationWire struct {
	preparationJSON
	SearchText []string `json:"searchText"`
}

func (p Preparation) MarshalJSON() ([]byte, error) {
	w := preparationWire{preparationJSON: preparationJSON(p)}
	if p.Batch != nil {
		w.SearchText = make([]string, len(p.Batch.Events))
		for i, event := range p.Batch.Events {
			w.SearchText[i] = event.SearchText
		}
	}
	return json.Marshal(w)
}

func (p *Preparation) UnmarshalJSON(data []byte) error {
	var w preparationWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Batch == nil {
		if len(w.SearchText) != 0 {
			return errors.New("search evidence without prepared batch")
		}
	} else {
		if len(w.SearchText) != len(w.Batch.Events) {
			return errors.New("incomplete prepared search evidence")
		}
		for i := range w.Batch.Events {
			w.Batch.Events[i].SearchText = w.SearchText[i]
		}
	}
	*p = Preparation(w.preparationJSON)
	return nil
}

// PrepareOnce performs the same bounded parsing as Once without modifying the
// ledger, including when the source ends in an incomplete record. It is the
// preparation boundary for moving parser-child writes into hub-owned admission.
func (x *Indexer) PrepareOnce(ctx context.Context, sourceID, generation string) (Preparation, error) {
	var result Preparation
	n, err := x.onceWithSinks(ctx, sourceID, generation,
		func(_ context.Context, batch store.IndexBatch) error {
			result.Batch = &batch
			return nil
		},
		func(_ context.Context, src store.SourceState, checkpoint json.RawMessage) error {
			result.Scan = &ScanPreparation{Source: src, Checkpoint: checkpoint}
			return nil
		})
	if err != nil {
		return Preparation{}, err
	}
	result.Records = n
	return result, nil
}
