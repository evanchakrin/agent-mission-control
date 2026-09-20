package store

import (
	"context"
	"fmt"
)

const MaxIndexGroupSources = 8

// IndexBatchBudget is a conservative allowance for a prepared batch and its
// projection expansion. Collectors of prepared batches use the same bound as
// the transaction admission check.
func IndexBatchBudget(b IndexBatch) int64 { return indexBatchBudget(b) }

func indexBatchBudget(b IndexBatch) int64 {
	budget := int64(64<<10) + int64(len(b.ParserState))*2
	for _, e := range b.Events {
		budget += int64(len(e.SearchText)+len(e.Text)+len(e.Data))*4 + 2048
	}
	for _, u := range b.Usage {
		budget += int64(len(u.Evidence)+len(u.Model)+len(u.AgentID))*4 + 2048
	}
	return budget
}

// CommitIndexGroup amortizes a FULL commit across a bounded set of independent
// source batches. Every checkpoint and derived contribution commits together,
// or none does. Captured raw receipts remain independent of indexing progress.
// Callers must not report any batch as indexed until this returns successfully.
func (s *Store) CommitIndexGroup(ctx context.Context, batches []IndexBatch) error {
	if len(batches) == 0 || len(batches) > MaxIndexGroupSources {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(batches))
	var budget int64
	for _, b := range batches {
		if seen[b.SourceID] {
			return fmt.Errorf("%w: repeated source in index group", ErrInvalid)
		}
		seen[b.SourceID] = true
		n := indexBatchBudget(b)
		if n < 0 || n > (32<<20)-budget {
			return fmt.Errorf("%w: index group exceeds bounded expansion allowance", ErrInvalid)
		}
		budget += n
	}
	s.capacityMu.Lock()
	err := s.capacity(budget)
	if err == nil {
		s.pendingBytes += budget
	}
	s.capacityMu.Unlock()
	if err != nil {
		return err
	}
	defer func() { s.capacityMu.Lock(); s.pendingBytes -= budget; s.capacityMu.Unlock() }()
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, b := range batches {
		if err := s.commitIndex(ctx, b, tx); err != nil {
			return fmt.Errorf("index group batch %d: %w", i, err)
		}
	}
	return tx.Commit()
}
