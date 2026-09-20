package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

type UsageContributionRow struct {
	Recorded        UsageObservation `json:"recorded"`
	Counted         bool             `json:"counted"`
	ExcludedByProof string           `json:"excludedByProof,omitempty"`
	OwnerSessionID  string           `json:"ownerSessionId,omitempty"`
	ExclusionKind   string           `json:"exclusionKind,omitempty"`
}

type UsageContributionPage struct {
	Items      []UsageContributionRow `json:"items"`
	NextCursor string                 `json:"nextCursor,omitempty"`
	Snapshot   string                 `json:"snapshot"`
	// Current selections may not cover every duplicate or inherited baseline.
	Scope string `json:"scope"`
}

// UsageContributions reads raw observations and decisions in one transaction.
// Its snapshot conservatively pins the change head, including other sources:
// an owner source may change without the excluded session itself changing.
func (s *Store) UsageContributions(ctx context.Context, id, after string, limit int, snapshot string) (UsageContributionPage, error) {
	page := UsageContributionPage{Items: []UsageContributionRow{}, Scope: "verified-usage-exclusions-only"}
	if !validID(id) || len(after) > 512 || len(snapshot) > 64 || limit < 1 || limit > 100 || (after != "" && snapshot == "") {
		return page, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id=?`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			err = ErrNotFound
		}
		return page, err
	}
	var head int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM changes`).Scan(&head); err != nil {
		return page, err
	}
	raw, _ := json.Marshal([]string{"usage-contributions-v1", s.epoch, id, strconv.FormatInt(head, 10)})
	hash := sha256.Sum256(raw)
	page.Snapshot = hex.EncodeToString(hash[:])
	if snapshot != "" && snapshot != page.Snapshot {
		return UsageContributionPage{}, ErrHistoryChanged
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.observation FROM usage_observations u JOIN sessions i ON i.id=u.session_id AND i.source_id=u.source_id AND i.generation=u.generation AND COALESCE(json_extract(i.projection,'$.projectionRevision'),'')=u.projection_revision WHERE u.session_id=? AND u.id>? ORDER BY u.id LIMIT ?`, id, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var encoded []byte
		if err = rows.Scan(&encoded); err != nil {
			rows.Close()
			return page, err
		}
		var u UsageObservation
		if err = json.Unmarshal(encoded, &u); err != nil {
			rows.Close()
			return page, err
		}
		page.Items = append(page.Items, UsageContributionRow{Recorded: u, Counted: true})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = page.Items[len(page.Items)-1].Recorded.ID
	}
	for i := range page.Items {
		exclusion, active, err := currentUsageContributionExclusion(ctx, tx, page.Items[i].Recorded.ID, s.epoch)
		if err != nil {
			return UsageContributionPage{}, err
		}
		if active {
			page.Items[i].Counted = false
			page.Items[i].ExcludedByProof = exclusion.ProofID
			page.Items[i].OwnerSessionID = exclusion.OwnerSessionID
			page.Items[i].ExclusionKind = exclusion.Kind
		}
	}
	return page, nil
}
