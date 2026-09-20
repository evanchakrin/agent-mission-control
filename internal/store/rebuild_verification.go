package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"
)

func (s *Store) migrateRebuildVerification(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 5 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `ALTER TABLE projection_revisions ADD COLUMN verification BLOB NOT NULL DEFAULT '{}'; PRAGMA user_version=5;`); err != nil {
		return err
	}
	return tx.Commit()
}

type VerificationProgress struct {
	Phase      string   `json:"phase"`
	EventAfter int64    `json:"eventAfter"`
	Events     int64    `json:"events"`
	UsageAfter string   `json:"usageAfter"`
	Tokens     [4]int64 `json:"tokens"`
	TailOffset int64    `json:"tailOffset"`
}

type verificationCheckpoint = VerificationProgress

// VerifyRebuildBatch reads at most 256 event ranges, 256 narrow usage rows and
// 4 MiB of a partial raw tail. Its tiny checkpoint commits independently. Reads
// hold no writer lock; concurrent verifiers use a checkpoint CAS to avoid double
// addition. Frozen staging rejects all new parser writes until publication.
func (s *Store) VerifyRebuildBatch(ctx context.Context, id string) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE projection_revisions SET state='verifying',updated_at=? WHERE revision=? AND state='building'`, stamp(time.Now()), id)
	s.writeMu.Unlock()
	if err != nil {
		return false, err
	}
	r, err := s.ProjectionRevision(ctx, id)
	if err != nil {
		return false, err
	}
	if r.State == "ready" || r.State == "active" || r.State == "retired" {
		return true, nil
	}
	if r.State != "verifying" {
		return false, ErrConflict
	}
	if r.IndexedOffset < 0 || r.IndexedOffset > r.TargetOffset {
		return false, ErrInvalid
	}
	var prior []byte
	if err = s.db.QueryRowContext(ctx, `SELECT verification FROM projection_revisions WHERE revision=?`, id).Scan(&prior); err != nil {
		return false, err
	}
	var cp verificationCheckpoint
	if len(prior) > 4096 || json.Unmarshal(prior, &cp) != nil || bytes.Equal(bytes.TrimSpace(prior), []byte("null")) {
		return false, fmt.Errorf("%w: damaged verification checkpoint", ErrInvalid)
	}
	if cp.Phase == "" {
		cp.Phase = "events"
	}
	if cp.EventAfter < 0 || cp.Events < 0 || cp.Events > r.Session.EventCount || len(cp.UsageAfter) > 512 || cp.TailOffset < 0 || cp.TailOffset > r.TargetOffset-r.IndexedOffset {
		return false, fmt.Errorf("%w: verification checkpoint bounds", ErrInvalid)
	}
	for _, n := range cp.Tokens {
		if n < 0 {
			return false, ErrInvalid
		}
	}
	if cp.Phase == "events" {
		rows, err := s.db.QueryContext(ctx, `SELECT seq,source_offset,raw_length FROM events WHERE source_id=? AND generation=? AND projection_revision=? AND seq>? ORDER BY seq LIMIT 256`, r.SourceID, r.Generation, r.Revision, cp.EventAfter)
		if err != nil {
			return false, err
		}
		count := 0
		for rows.Next() {
			var seq, offset, length int64
			if err = rows.Scan(&seq, &offset, &length); err != nil {
				rows.Close()
				return false, err
			}
			if offset < 0 || length < 0 || offset > r.IndexedOffset || length > r.IndexedOffset-offset {
				rows.Close()
				return false, fmt.Errorf("%w: staged event outside indexed evidence", ErrInvalid)
			}
			cp.EventAfter = seq
			cp.Events++
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if count < 256 {
			cp.Phase = "usage"
		} else {
			return s.saveVerification(ctx, r, prior, cp, false)
		}
	}
	if cp.Phase == "usage" {
		rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(json_extract(observation,'$.tokensIn'),0),COALESCE(json_extract(observation,'$.tokensCache'),0),COALESCE(json_extract(observation,'$.tokensCacheWrite'),0),COALESCE(json_extract(observation,'$.tokensOut'),0) FROM usage_observations WHERE source_id=? AND generation=? AND projection_revision=? AND id>? ORDER BY id LIMIT 256`, r.SourceID, r.Generation, r.Revision, cp.UsageAfter)
		if err != nil {
			return false, err
		}
		count := 0
		for rows.Next() {
			var id string
			var tokens [4]int64
			if err = rows.Scan(&id, &tokens[0], &tokens[1], &tokens[2], &tokens[3]); err != nil {
				rows.Close()
				return false, err
			}
			for i, n := range tokens {
				if n < 0 || cp.Tokens[i] > math.MaxInt64-n {
					rows.Close()
					return false, fmt.Errorf("%w: verification token overflow", ErrInvalid)
				}
				cp.Tokens[i] += n
			}
			cp.UsageAfter = id
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if count < 256 {
			cp.Phase = "tail"
		} else {
			return s.saveVerification(ctx, r, prior, cp, false)
		}
	}
	if cp.Phase != "tail" {
		return false, fmt.Errorf("%w: unknown verification phase", ErrInvalid)
	}
	if cp.Events != r.Session.EventCount || cp.Tokens != [4]int64{r.Session.TokensIn, r.Session.TokensCache, r.Session.TokensCacheWrite, r.Session.TokensOut} {
		return false, fmt.Errorf("%w: staged projection aggregate mismatch", ErrInvalid)
	}
	remaining := r.TargetOffset - r.IndexedOffset - cp.TailOffset
	if remaining > 0 {
		raw, err := s.OpenSource(ctx, r.SourceID, r.Generation, r.IndexedOffset+cp.TailOffset)
		if err != nil {
			return false, err
		}
		reader := io.LimitReader(raw, min(remaining, 4<<20))
		buf := make([]byte, 64<<10)
		for {
			if err = ctx.Err(); err != nil {
				raw.Close()
				return false, err
			}
			n, e := reader.Read(buf)
			if bytes.IndexByte(buf[:n], '\n') >= 0 {
				raw.Close()
				return false, fmt.Errorf("%w: complete records remain beyond rebuild checkpoint", ErrConflict)
			}
			cp.TailOffset += int64(n)
			if e == io.EOF {
				break
			}
			if e != nil {
				raw.Close()
				return false, e
			}
		}
		if err = raw.Close(); err != nil {
			return false, err
		}
	}
	return s.saveVerification(ctx, r, prior, cp, cp.TailOffset == r.TargetOffset-r.IndexedOffset)
}

func (s *Store) saveVerification(ctx context.Context, r ProjectionRevision, prior []byte, cp verificationCheckpoint, done bool) (bool, error) {
	data, err := json.Marshal(cp)
	if err != nil {
		return false, err
	}
	state := "verifying"
	if done {
		state = "ready"
		r.Session.Completeness = "indexed-source"
		if r.IndexedOffset < r.TargetOffset {
			r.Session.Completeness = "indexed-source-partial"
		}
	}
	projection, err := json.Marshal(r.Session)
	if err != nil {
		return false, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	if done {
		if err := validateHookCheckpoint(ctx, s.db, r); err != nil {
			return false, err
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE projection_revisions SET verification=?,state=?,projection=?,error='',updated_at=? WHERE revision=? AND state='verifying' AND CAST(verification AS BLOB)=?`, data, state, projection, stamp(time.Now()), r.Revision, prior)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, ErrConflict
	}
	return done, nil
}
