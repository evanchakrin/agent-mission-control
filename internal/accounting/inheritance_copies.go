package accounting

import (
	"context"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// InspectCapturedForkBaseline shares one scan budget across all captured copies.
// A contribution owner need not contain the parent's complete source history.
func InspectCapturedForkBaseline(ctx context.Context, s *store.Store, childID string, maxPages int) (ForkBaselineInspection, error) {
	result := ForkBaselineInspection{State: "unverified", Reason: "Fork parent history has not been captured"}
	if maxPages < 1 || maxPages > 200 {
		return result, store.ErrInvalid
	}
	child, err := s.GetSession(ctx, childID)
	if err != nil {
		return result, err
	}
	if child.Provider != "codex" || child.ForkedFromID == "" {
		result.Reason = "No explicit Codex fork origin recorded"
		return result, nil
	}
	after := ""
	for {
		ids, err := s.NativeSessionCopies(ctx, child.MachineID, child.ForkedFromID, after, 20)
		if err != nil {
			return result, err
		}
		for _, parentID := range ids {
			if result.Pages >= maxPages {
				return result, ErrInheritanceScanIncomplete
			}
			inspection, err := InspectForkBaseline(ctx, s, childID, parentID, maxPages-result.Pages)
			result.Pages += inspection.Pages
			if err != nil {
				return result, err
			}
			if inspection.Match != nil {
				inspection.Pages = result.Pages
				return inspection, nil
			}
			result.Reason = inspection.Reason
			after = parentID
		}
		if len(ids) < 20 {
			return result, nil
		}
	}
}
