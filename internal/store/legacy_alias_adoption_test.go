package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestLegacyAliasAdoptsUneditedImportedPlaceholder(t *testing.T) {
	for _, ownerEdited := range []bool{false, true} {
		t.Run(map[bool]string{false: "imported", true: "owner-wins"}[ownerEdited], func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			b.Session.ID = "arriving"
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			key := "R:erp:claude:late-source"
			if err := s.ImportLegacyMetadata(ctx, nil, map[string]json.RawMessage{key: json.RawMessage(`{"archived":true,"note":"old note","agentNames":{"main":"Old name"}}`)}); err != nil {
				t.Fatal(err)
			}
			if ownerEdited {
				name := "New owner name"
				if _, err := s.PatchMetadata(ctx, "arriving", MetadataPatch{OperationID: "owner-edit", Name: &name}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := s.PutLegacyAlias(ctx, key, "arriving"); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.ResolveAlias(ctx, key)
			if err != nil || id != "arriving" {
				t.Fatal(id, err)
			}
			m, err := s.GetMetadata(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if ownerEdited {
				if m.Name != "New owner name" || m.Archived || m.Note != "" {
					t.Fatal("owner edit overwritten", m)
				}
			} else if !m.Archived || m.Note != "old note" || m.Revision != 1 {
				t.Fatal("import lost", m)
			}
			original, err := s.GetMetadata(ctx, key)
			if err != nil || !original.Archived {
				t.Fatal("original evidence lost", original, err)
			}
			if err := s.ImportLegacyMetadata(ctx, nil, map[string]json.RawMessage{key: json.RawMessage(`{"archived":true,"note":"old note"}`)}); err != nil {
				t.Fatal("import replay after adoption", err)
			}
			after, err := s.GetMetadata(ctx, id)
			if err != nil || after.Archived != m.Archived || after.Name != m.Name || after.Note != m.Note || after.Revision != m.Revision {
				t.Fatal("import replay changed organization", after, err)
			}
			if err := s.PutLegacyAlias(ctx, key, "different"); !errors.Is(err, ErrConflict) {
				t.Fatal("real alias reassigned", err)
			}
		})
	}
}

func TestLegacyAliasRefusesEditedPlaceholder(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	key := "R:erp:claude:edited"
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Session.ID = key
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyMetadata(ctx, nil, map[string]json.RawMessage{key: json.RawMessage(`{"archived":true}`)}); err != nil {
		t.Fatal(err)
	}
	note := "Owner edit must be reconciled explicitly"
	if _, err := s.PatchMetadata(ctx, key, MetadataPatch{Revision: 1, OperationID: "edit-placeholder", Note: &note}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLegacyAlias(ctx, key, "arriving"); !errors.Is(err, ErrConflict) {
		t.Fatal("edited placeholder adopted", err)
	}
}

func TestLegacyAliasRefusesUnprovenPlaceholder(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.PutLegacyAlias(ctx, "old", "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLegacyAlias(ctx, "old", "arriving"); !errors.Is(err, ErrConflict) {
		t.Fatal("unproven alias adopted", err)
	}
}
