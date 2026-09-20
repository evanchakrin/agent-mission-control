package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestCatalogIDPageIsBoundedAndLossless(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 130; i++ {
		id := fmt.Sprintf("catalog-%03d", i)
		b, _ := json.Marshal(map[string]any{"id": id, "rates": []any{}})
		if err = s.PutRateCatalog(ctx, id, b); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.RateCatalogIDs(ctx, "")
	if err != nil || len(first) != 101 || first[0] != "catalog-000" || first[100] != "catalog-100" {
		t.Fatal(first, err)
	}
	second, err := s.RateCatalogIDs(ctx, first[99])
	if err != nil || len(second) != 30 || second[0] != first[100] || second[29] != "catalog-129" {
		t.Fatal(second, err)
	}
	end, err := s.RateCatalogIDs(ctx, second[29])
	if err != nil || len(end) != 0 {
		t.Fatal(end, err)
	}
}
