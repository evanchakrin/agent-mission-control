package indexer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Run has one parser worker. Remove the specific startup issue only after that
// worker publishes the replacement. No whole-corpus rescan on each small job.
func (x *Indexer) rebuiltVersion(before store.SourceState) {
	var state struct {
		Version *string `json:"version"`
	}
	err := json.Unmarshal(before.ParserState, &state)
	valid := err == nil && len(before.ParserState) > 0 && string(before.ParserState) != "null"
	version := ""
	if state.Version != nil {
		version = *state.Version
	}
	if valid && (version == parser.Version || (version == "" && before.IndexedOffset == 0)) {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.status.VersionIssues > 0 {
		x.status.VersionIssues--
	}
	if v := x.status.VersionExample; v != nil && v.SourceID == before.Source.SourceID && v.Generation == before.Source.Generation {
		x.status.VersionExample = nil
	}
}

// RebuildOnce commits at most one normal bounded parser batch. Verification and
// publication are independently restartable states in the same durable ledger.
func (x *Indexer) RebuildOnce(ctx context.Context, id string) (int64, error) {
	return x.RebuildPrepared(ctx, id, x.PrepareRebuild)
}

// RebuildPrepared keeps all ledger mutations in the hub while parsing can run
// in the bounded child. Verification and publication retain their existing
// durable state machine and compare-and-swap guards.
func (x *Indexer) RebuildPrepared(ctx context.Context, id string, prepare func(context.Context, string) (Preparation, error)) (int64, error) {
	r, err := x.Store.ProjectionRevision(ctx, id)
	if err != nil {
		return 0, err
	}
	if r.ParserVersion != parser.Version {
		return 0, fmt.Errorf("%w: requested rebuild parser is not this binary", parser.ErrRebuildRequired)
	}
	var n int64
	if r.State == "building" {
		prepared, e := prepare(ctx, id)
		if e != nil {
			return 0, e
		}
		n, err = x.publishPreparation(ctx, prepared)
		if err != nil {
			return n, err
		}
		r, err = x.Store.ProjectionRevision(ctx, id)
		if err != nil {
			return n, err
		}
		if n > 0 && r.IndexedOffset < r.TargetOffset {
			return n, nil
		}
		state, e := parser.DecodeState(r.ParserState)
		if e != nil {
			return n, e
		}
		if state.ScanOffset > r.IndexedOffset && state.ScanOffset < r.TargetOffset {
			return n, nil
		}
		r.State = "verifying"
	}
	if r.State == "verifying" {
		var done bool
		if done, err = x.Store.VerifyRebuildBatch(ctx, id); err != nil {
			return n, err
		}
		if !done {
			return n, nil
		}
		r.State = "ready"
	}
	if r.State == "ready" {
		err = x.Store.PublishRebuild(ctx, id)
	}
	return n, err
}

// PrepareRebuild never updates either the active projection or staged ledger.
func (x *Indexer) PrepareRebuild(ctx context.Context, id string) (Preparation, error) {
	src, job, err := x.Store.RebuildSource(ctx, id)
	if err != nil {
		return Preparation{}, err
	}
	if job.ParserVersion != parser.Version {
		return Preparation{}, fmt.Errorf("%w: requested rebuild parser is not this binary", parser.ErrRebuildRequired)
	}
	var result Preparation
	n, err := x.indexSourceWithSinks(ctx, src, job.Session.ID,
		func(_ context.Context, batch store.IndexBatch) error { result.Batch = &batch; return nil },
		func(_ context.Context, source store.SourceState, checkpoint json.RawMessage) error {
			result.Scan = &ScanPreparation{Source: source, Checkpoint: checkpoint}
			return nil
		})
	if err != nil {
		return Preparation{}, err
	}
	result.Records = n
	return result, nil
}

type VersionIssue struct {
	SessionID         string `json:"sessionId,omitempty"`
	SourceID          string `json:"sourceId"`
	Generation        string `json:"generation"`
	CheckpointVersion string `json:"checkpointVersion"`
	RequiredVersion   string `json:"requiredVersion"`
	Code              string `json:"code"`
	Detail            string `json:"detail"`
}
type VersionPage struct {
	Checked int            `json:"checked"`
	Issues  []VersionIssue `json:"issues"`
	Next    string         `json:"next,omitempty"`
}

// CheckVersions is a read-only paginated startup/doctor audit. It includes
// caught-up sources which the normal pending-work scheduler deliberately skips.
// It does not reset checkpoints, erase projections, or attempt an unsafe replay.
func (x *Indexer) CheckVersions(ctx context.Context, after string, limit int) (VersionPage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	result := VersionPage{Issues: []VersionIssue{}}
	sources, err := x.Store.SourceCheckpointVersions(ctx, after, limit)
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		result.Checked++
		if source.Valid && (source.Version == parser.Version || (source.Version == "" && source.IndexedOffset == 0)) {
			continue
		}
		code := "damaged-checkpoint"
		detail := "Invalid parser checkpoint; existing projection retained"
		if source.Valid {
			code = "rebuild-required"
			detail = "Parser version differs or is unknown; staged projection rebuild required; existing projection retained"
		}
		result.Issues = append(result.Issues, VersionIssue{SessionID: source.SessionID, SourceID: source.SourceID, Generation: source.Generation, CheckpointVersion: source.Version, RequiredVersion: parser.Version, Code: code, Detail: detail})
	}
	if len(sources) == limit {
		result.Next = store.CheckpointVersionCursor(sources[len(sources)-1])
	}
	return result, nil
}
