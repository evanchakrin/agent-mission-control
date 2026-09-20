package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"time"
)

func EnableAutomatic(ctx context.Context, s *store.Store, sessionID, catalogID, billingContext string) error {
	return EnableAutomaticAt(ctx, s, sessionID, catalogID, billingContext, nil)
}

// A dated comparison is an explicit alternative to historical automatic pricing.
// It preserves historical snapshots and never selects a comparison as a charge.
func EnableAutomaticAt(ctx context.Context, s *store.Store, sessionID, catalogID, billingContext string, comparisonAt *time.Time) error {
	if err := validateAutomaticCatalog(ctx, s, catalogID, billingContext, comparisonAt); err != nil {
		return err
	}
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return err
	}
	return s.SetPricingPolicyAt(ctx, sessionID, catalogID, billingContext, comparisonAt)
}

func EnableComparisonDefault(ctx context.Context, s *store.Store, p store.ComparisonDefault) error {
	if p.ComparisonAt.IsZero() {
		return store.ErrInvalid
	}
	if err := validateAutomaticCatalog(ctx, s, p.CatalogID, p.Context, &p.ComparisonAt); err != nil {
		return err
	}
	return s.SetComparisonDefault(ctx, &p)
}

func validateAutomaticCatalog(ctx context.Context, s *store.Store, catalogID, billingContext string, comparisonAt *time.Time) error {
	if err := s.InitializePricingQueue(ctx); err != nil {
		return err
	}
	raw, err := s.RateCatalog(ctx, catalogID)
	if err != nil {
		return err
	}
	var c Catalog
	if err = json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if err = validateCatalogUse(c, comparisonAt); err != nil {
		return err
	}
	found := false
	for _, r := range c.Rates {
		if r.Context == billingContext && (comparisonAt == nil || (!comparisonAt.Before(r.EffectiveFrom) && (r.EffectiveTo == nil || comparisonAt.Before(*r.EffectiveTo)))) {
			found = true
		}
	}
	if !found {
		return store.ErrInvalid
	}
	return nil
}

// One session at a time, with bounded usage pages and a request deadline. Failed
// jobs remain durable and cannot monopolize the queue ahead of healthy sessions.
func PriceNext(ctx context.Context, s *store.Store) (bool, error) {
	j, err := s.NextPricingJob(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	done, problem := priceBatch(work, s, j)
	cancel()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if done || problem != nil {
		return true, s.FinishPricingJob(ctx, j, problem)
	}
	return true, nil
}

func Run(ctx context.Context, s *store.Store) error {
	return RunReporting(ctx, s, nil)
}

func RunReporting(ctx context.Context, s *store.Store, onContention func(error)) error {
	for {
		err := run(ctx, s)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !store.IsContention(err) {
			return err
		}
		if onContention != nil {
			onContention(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func run(ctx context.Context, s *store.Store) error {
	if err := s.InitializePricingQueue(ctx); err != nil {
		return err
	}
	for {
		worked, err := PriceNext(ctx, s)
		if err != nil {
			return err
		}
		delay := 2 * time.Second
		if worked {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
