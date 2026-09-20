package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDelegationMatchesCompleteScopesAndRevision(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	var anchors []int64
	for i := 0; i < 8; i++ {
		src := testSource()
		src.SourceID = fmt.Sprint("match-source-", i)
		if i == 3 {
			src.MachineID = "other-machine"
		}
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprint("match-session-", i)
		prompt := strings.Repeat("whole instruction ", 20) + "the actual ending"
		if i == 4 {
			prompt += " different"
		}
		full, _ := json.Marshal(map[string]string{"prompt": prompt})
		e := Event{ID: fmt.Sprint("match-call-", i), Kind: "tool-call", AgentID: "caller", SourceLength: 2, Text: "same short preview", SearchText: string(full), Data: json.RawMessage(`{"tool":"Task"}`)}
		if i == 5 {
			e.SearchText = ""
		}
		b.Events = []Event{e}
		if i == 0 {
			e.ID = "same-session-extra"
			b.Events = append(b.Events, e)
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		var seq int64
		if err := s.db.QueryRowContext(ctx, `SELECT seq FROM events WHERE id=?`, fmt.Sprint("match-call-", i)).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		anchors = append(anchors, seq)
		if i == 2 {
			yes := true
			if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: "archive-match", Archived: &yes}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM delegation_tasks_v1 WHERE event_sequence=?`, anchors[6]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET generation='rewritten' WHERE id='match-session-7'`); err != nil {
		t.Fatal(err)
	}
	page, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], 0, 1, "")
	if err != nil || page.State != "known" || len(page.Matches) != 1 || page.Matches[0].SessionID != "match-session-1" || page.NextSequence != anchors[1] {
		t.Fatal(page, err)
	}
	if page.Coverage != "indexed-fingerprints-only" || page.Matches[0].CallerAgentID != "caller" || len(page.Matches[0].SessionSnapshot) != 64 {
		t.Fatal(page)
	}
	second, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], page.NextSequence, 1, page.Snapshot)
	if err != nil || len(second.Matches) != 1 || second.Matches[0].SessionID != "match-session-2" || !second.Matches[0].Archived {
		t.Fatal(second, err)
	}
	name := "live organization name"
	if _, err := s.PatchMetadata(ctx, "match-session-3", MetadataPatch{OperationID: "rename-match", Name: &name}); err != nil {
		t.Fatal(err)
	}
	third, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], second.NextSequence, 1, page.Snapshot)
	if err != nil || len(third.Matches) != 1 || third.Matches[0].SessionID != "match-session-3" || third.Matches[0].MachineID != "other-machine" || third.Matches[0].Title != name || third.NextSequence != 0 {
		t.Fatal(third, err)
	}
	for i, want := range map[int]string{5: "missing-full-text", 6: "needs-reindex"} {
		missing, err := s.DelegationMatches(ctx, fmt.Sprint("match-session-", i), anchors[i], 0, 20, "")
		if err != nil || missing.State != want || len(missing.Matches) != 0 {
			t.Fatal(missing, err)
		}
	}
	if _, err := s.DelegationMatches(ctx, "match-session-1", anchors[1], page.NextSequence, 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cross-anchor cursor accepted", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET text='changed' WHERE id='match-call-2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], page.NextSequence, 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed fingerprint retained cursor", err)
	}
	current, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], 0, 20, "")
	if err != nil || len(current.Matches) != 2 {
		t.Fatal(current, err)
	}
	oldEpoch := s.epoch
	s.epoch = "fixture-restored-epoch"
	if _, err := s.DelegationMatches(ctx, "match-session-0", anchors[0], 0, 20, current.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("restore retained cursor", err)
	}
	s.epoch = oldEpoch
}
