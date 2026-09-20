package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

type WorkRequest struct {
	Prepare    bool         `json:"prepare,omitempty"`
	Sources    []SourceWork `json:"sources,omitempty"`
	SourceID   string       `json:"sourceId"`
	Generation string       `json:"generation"`
	Revision   string       `json:"revision,omitempty"`
}
type WorkResult struct {
	Prepared *Preparation   `json:"prepared,omitempty"`
	Sources  []SourceResult `json:"sources,omitempty"`
	Records  int64          `json:"records"`
	Error    string         `json:"error,omitempty"`
}

// ProcessWorker is a single persistent parser child. Its OS Job Object bounds
// memory and ensures descendants cannot survive hub shutdown. The child reads
// immutable chunks; preparation responses carry bounded derived projections.
type ProcessWorker struct {
	Executable, DataDir string
	Lifetime            context.Context
	mu                  sync.Mutex
	child               *platform.Worker
	input               io.WriteCloser
	output              *WorkerDecoder
}

func (p *ProcessWorker) Close() error { p.mu.Lock(); defer p.mu.Unlock(); return p.close() }
func (p *ProcessWorker) close() error {
	if p.input != nil {
		_ = p.input.Close()
		p.input = nil
	}
	if p.child != nil {
		err := p.child.Stop()
		select {
		case <-p.child.Done():
		case <-time.After(5 * time.Second):
			return errors.New("parser did not exit after forced shutdown")
		}
		p.child = nil
		p.output = nil
		return err
	}
	return nil
}
func (p *ProcessWorker) start(ctx context.Context) error {
	cmd := exec.Command(p.Executable, "parser-worker", "--data-dir", p.DataDir)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	lifetime := p.Lifetime
	if lifetime == nil {
		lifetime = ctx
	}
	child, err := platform.StartWorker(lifetime, platform.WorkerSpec{Command: cmd, MemoryBytes: 192 << 20})
	if err != nil {
		in.Close()
		out.Close()
		return err
	}
	p.child = child
	p.input = in
	p.output = NewWorkerDecoder(out, MaxWorkerResponseBytes)
	return nil
}
func (p *ProcessWorker) Process(ctx context.Context, sourceID, generation string) (int64, error) {
	return p.request(ctx, WorkRequest{SourceID: sourceID, Generation: generation})
}

// Prepare returns unpublished work. Only a subsequent hub commit can turn its
// record count into durable progress.
func (p *ProcessWorker) Prepare(ctx context.Context, sourceID, generation string) (Preparation, error) {
	return p.prepare(ctx, WorkRequest{Prepare: true, SourceID: sourceID, Generation: generation})
}

func (p *ProcessWorker) PrepareRebuild(ctx context.Context, revision string) (Preparation, error) {
	return p.prepare(ctx, WorkRequest{Prepare: true, Revision: revision})
}

func (p *ProcessWorker) prepare(ctx context.Context, request WorkRequest) (Preparation, error) {
	if err := request.ValidatePreparation(); err != nil {
		return Preparation{}, err
	}
	r, err := p.exchange(ctx, request)
	if err != nil {
		return Preparation{}, err
	}
	if r.Prepared == nil || r.Records != 0 || len(r.Sources) != 0 {
		return Preparation{}, errors.New("parser returned invalid preparation response")
	}
	prepared := r.Prepared
	if prepared.Batch != nil {
		if prepared.Scan != nil || prepared.Records <= 0 {
			return Preparation{}, errors.New("parser returned invalid prepared batch")
		}
		if request.Revision != "" {
			if prepared.Batch.ProjectionRevision != request.Revision {
				return Preparation{}, errors.New("parser returned another revision")
			}
		} else if prepared.Batch.SourceID != request.SourceID || prepared.Batch.Generation != request.Generation {
			return Preparation{}, errors.New("parser returned another source")
		}
	} else if prepared.Records != 0 {
		return Preparation{}, errors.New("parser returned record count without batch")
	} else if prepared.Scan != nil {
		if request.Revision != "" {
			if prepared.Scan.Source.ProjectionRevision != request.Revision {
				return Preparation{}, errors.New("parser returned scan for another revision")
			}
		} else if prepared.Scan.Source.Source.SourceID != request.SourceID || prepared.Scan.Source.Source.Generation != request.Generation {
			return Preparation{}, errors.New("parser returned scan for another source")
		}
	}
	return *r.Prepared, nil
}
func (p *ProcessWorker) Rebuild(ctx context.Context, revision string) (int64, error) {
	return p.request(ctx, WorkRequest{Revision: revision})
}
func (p *ProcessWorker) ProcessGroup(ctx context.Context, sources []SourceWork) ([]SourceResult, error) {
	r, err := p.exchange(ctx, WorkRequest{Sources: sources})
	if err == nil && len(r.Sources) != len(sources) {
		err = errors.New("parser returned an incomplete source group")
	}
	return r.Sources, err
}
func (p *ProcessWorker) request(ctx context.Context, request WorkRequest) (int64, error) {
	r, err := p.exchange(ctx, request)
	return r.Records, err
}
func (p *ProcessWorker) exchange(ctx context.Context, request WorkRequest) (WorkResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.child == nil {
		if err := p.start(ctx); err != nil {
			return WorkResult{}, err
		}
	}
	type response struct {
		result WorkResult
		err    error
	}
	reply := make(chan response, 1)
	decoder := p.output
	input := p.input
	go func() {
		var r WorkResult
		err := json.NewEncoder(input).Encode(request)
		if err == nil {
			err = decoder.Decode(&r)
		}
		reply <- response{r, err}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = p.close()
		return WorkResult{}, ctx.Err()
	case <-timer.C:
		_ = p.close()
		return WorkResult{}, errors.New("parser did not respond within 30 seconds; child terminated")
	case r := <-reply:
		if r.err != nil {
			_ = p.close()
			return WorkResult{}, fmt.Errorf("parser child exited before checkpoint: %w", r.err)
		}
		if r.result.Error != "" {
			return WorkResult{}, errors.New(r.result.Error)
		}
		return r.result, nil
	}
}
