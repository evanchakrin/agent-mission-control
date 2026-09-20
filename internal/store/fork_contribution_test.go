package store

import (
	"context"
	"errors"
	"testing"
)

func TestForkBaselineAncestryRejectsCyclesAndMissingEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	child := Session{NativeID: "child", MachineID: "machine"}
	for _, tc := range []struct {
		name     string
		parent   Session
		conflict bool
	}{
		{"root", Session{NativeID: "parent"}, false},
		{"self-cycle", Session{NativeID: "child"}, true},
		{"missing-ancestor", Session{NativeID: "parent", ForkedFromID: "missing"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyForkAncestry(ctx, tx, child, tc.parent)
			if tc.conflict && !errors.Is(err, ErrConflict) || !tc.conflict && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForkBaselineAncestryWalkDetectsMultiSessionCycle(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	// Two actual catalog ancestors, initially ending at a root.
	if _, err := s.db.Exec(`UPDATE sessions SET machine_id='machine',provider='codex',projection=json_set(projection,'$.machineId','machine','$.provider','codex','$.forkedFromId',CASE id WHEN 's-000000' THEN 's-000001' ELSE '' END)`); err != nil {
		t.Fatal(err)
	}
	check := func(wantConflict bool) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		parent, err := canonicalProofSession(ctx, tx, "machine", "s-000000")
		if err != nil {
			t.Fatal(err)
		}
		err = verifyForkAncestry(ctx, tx, Session{NativeID: "child", MachineID: "machine"}, parent)
		if wantConflict && !errors.Is(err, ErrConflict) || !wantConflict && err != nil {
			t.Fatal("ancestry walk", err)
		}
	}
	check(false)
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.forkedFromId','s-000000') WHERE id='s-000001'`); err != nil {
		t.Fatal(err)
	}
	check(true)
}
