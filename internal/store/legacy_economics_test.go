package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyEconomicsPagesRetainDuplicatesMalformedAndPartialRecords(t *testing.T) {
	s := openTestStore(t, Options{})
	dir := filepath.Join(s.dir, "legacy-assets")
	os.MkdirAll(dir, 0700)
	lines := []string{"{\"at\":1}\r\n", "{\"at\":1}\r\n", "malformed\n", "{\"partial\":"}
	file := filepath.Join(dir, "econ-history.jsonl")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "")), 0600); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	var offset int64
	for i, line := range lines {
		page, err := s.LegacyEconomics(context.Background(), cursor, 1)
		if err != nil || len(page.Items) != 1 {
			t.Fatal(page, err)
		}
		item := page.Items[0]
		if item.Raw != line || item.Offset != offset || item.Length != len(line) || item.ValidObject != (i < 2) || item.Complete != (i < 3) {
			t.Fatal(item)
		}
		offset += int64(len(line))
		cursor = page.NextCursor
	}
	if cursor != "" {
		t.Fatal("unexpected remaining page")
	}
	first, err := s.LegacyEconomics(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LegacyEconomics(context.Background(), first.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed source accepted cursor", err)
	}
	if err = os.WriteFile(file, []byte(strings.Repeat("x", 70<<10)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LegacyEconomics(context.Background(), "", 1); err == nil {
		t.Fatal("oversized display record accepted")
	}
	f, err := s.OpenLegacyEconomics()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	if info.Size() != 70<<10 {
		t.Fatal("oversized evidence lost")
	}
}
