package store

import (
	"context"
	"errors"
	"testing"
)

func TestMachineLabelHistoryPagedImportRenameClear(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.ImportLegacyMachineLabels(ctx, map[string]string{"m": "Imported", "other": "Unrelated"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []MachineLabelMutation{{MachineID: "m", DisplayName: "Owner", Revision: 1, OperationID: "rename"}, {MachineID: "m", Revision: 2, OperationID: "clear"}} {
		if _, err := s.MutateMachineLabel(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MutateMachineLabel(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	var after int64
	for i, want := range []string{"Imported", "Owner", ""} {
		page, err := s.MachineLabelHistory(ctx, "m", after, 1)
		if err != nil || len(page.Items) != 1 {
			t.Fatal(page, err)
		}
		item := page.Items[0]
		if item.Revision != int64(i+1) || item.After.DisplayName != want || item.Before.Revision != int64(i) || item.After.MachineID != "m" || item.Before.MachineID != "m" {
			t.Fatal(item)
		}
		if i < 2 && page.Next != item.Revision || i == 2 && page.Next != 0 {
			t.Fatal("bad continuation", page)
		}
		after = item.Revision
	}
	page, err := s.MachineLabelHistory(ctx, "m", after, 1)
	if err != nil || len(page.Items) != 0 || page.Next != 0 {
		t.Fatal(page, err)
	}
	if _, err = s.MachineLabelHistory(ctx, "m", -1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
