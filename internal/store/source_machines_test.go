package store

import (
	"context"
	"testing"
)

func TestSourceOnlyMachineIsDiscoverableWithoutHeartbeat(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "unindexed raw history\n")
	p, err := s.CatalogMachines(ctx, SessionQuery{Limit: 1})
	if err != nil || len(p.Items) != 1 {
		t.Fatal("retained machine hidden", p, err)
	}
	i := p.Items[0]
	if i.ID != src.MachineID || i.Sessions != 0 || i.Accounting != nil || i.CostEstimate != nil {
		t.Fatal("invented indexed accounting", i)
	}
	archived := true
	p, err = s.CatalogMachines(ctx, SessionQuery{Archived: &archived, Limit: 1})
	if err != nil || len(p.Items) != 0 {
		t.Fatal("source-only machine bypassed session filter", p, err)
	}
}

func TestSourceOnlyMachineMergedPagination(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		src := testSource()
		src.MachineID = id
		src.SourceID = "source-" + id
		ingest(t, s, src, 0, "raw\n")
		if id == "b" {
			if err := s.CommitIndex(ctx, batch(src, 0, 4)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "a", DisplayName: "Retained owner label", OperationID: "rename-raw"}); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	for _, want := range []string{"a", "b", "c"} {
		page, err := s.CatalogMachines(ctx, SessionQuery{Limit: 1, Cursor: cursor})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != want {
			t.Fatal(want, page, err)
		}
		item := page.Items[0]
		if item.SourceOnly != (want != "b") {
			t.Fatal("wrong evidence classification", item)
		}
		if want == "a" && item.Name != "Retained owner label" {
			t.Fatal(item)
		}
		if (page.NextCursor != "") != (want != "c") {
			t.Fatal("incorrect continuation", page)
		}
		cursor = page.NextCursor
	}
}
