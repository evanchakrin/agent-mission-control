package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestHistoryCursorsRejectPublicationAndRestoreButAllowOrganization(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	page, err := s.ListEventsPinned(ctx, "session-1", 0, 1, "")
	if err != nil || len(page.Events) != 1 || page.Snapshot == "" {
		t.Fatal(page, err)
	}
	usage, err := s.GetUsagePinned(ctx, "session-1", "", 1, "")
	if err != nil || usage.Snapshot != page.Snapshot {
		t.Fatal(usage, err)
	}
	hits, err := s.Search(ctx, SearchQuery{Text: "baseline", Limit: 1})
	if err != nil || len(hits.Events) != 1 || hits.Events[0].HistorySnapshot != page.Snapshot {
		t.Fatal("search hit was not bound to its interpretation", hits, err)
	}
	if _, err = s.EventsAroundPinned(ctx, hits.Events[0].SessionID, hits.Events[0].Sequence, 1, 1, hits.Events[0].HistorySnapshot); err != nil {
		t.Fatal("current search anchor failed", err)
	}
	yes := true
	searchQuery := SearchQuery{Text: "baseline", Limit: 1}
	searchPage, err := s.SearchPinned(ctx, searchQuery, "")
	if err != nil || searchPage.Snapshot == "" {
		t.Fatal(searchPage, err)
	}
	if _, err = s.SearchPinned(ctx, SearchQuery{Text: "different"}, searchPage.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cursor crossed search text", err)
	}
	if _, err = s.SearchPinned(ctx, SearchQuery{Text: "baseline", AfterSequence: 1}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbound search continuation", err)
	}
	if _, err = s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "archive-cursor", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListEventsPinned(ctx, "session-1", page.Events[0].Sequence, 1, page.Snapshot); err != nil {
		t.Fatal("organization invalidated history", err)
	}
	if _, err = s.SearchPinned(ctx, searchQuery, searchPage.Snapshot); err != nil {
		t.Fatal("organization invalidated search", err)
	}
	if _, err = s.ListEventsPinned(ctx, "session-1", page.Events[0].Sequence, 1, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbound event continuation", err)
	}
	if _, err = s.GetUsagePinned(ctx, "session-1", usage.NextCursor, 1, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbound usage continuation", err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "second", "3")
	if err != nil {
		t.Fatal(err)
	}
	b := batch(testSource(), 0, 13)
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	b.Events = []Event{{ID: "new", SourceLength: 13}}
	b.Usage = []UsageObservation{{ID: "new", TokensIn: 1}}
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SearchPinned(ctx, searchQuery, searchPage.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("search pages crossed publication", err)
	}
	if _, err = s.ListEventsPinned(ctx, "session-1", page.Events[0].Sequence, 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("event cursor crossed revision", err)
	}
	if _, err = s.GetUsagePinned(ctx, "session-1", usage.NextCursor, 1, usage.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("usage cursor crossed revision", err)
	}
	if _, err = s.EventsAroundPinned(ctx, "session-1", page.Events[0].Sequence, 1, 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("replay cursor crossed revision", err)
	}
	if _, err = s.EventsAroundPinned(ctx, hits.Events[0].SessionID, hits.Events[0].Sequence, 1, 1, hits.Events[0].HistorySnapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("search anchor crossed publication", err)
	}
	page, err = s.ListEventsPinned(ctx, "session-1", 0, 1, "")
	if err != nil || len(page.Events) != 1 || page.Events[0].ProjectionRevision != r.Revision {
		t.Fatal(page, err)
	}
	dest := filepath.Join(t.TempDir(), "backup")
	freshSearch, err := s.SearchPinned(ctx, SearchQuery{Text: "baseline"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(ctx, dest, filepath.Join(t.TempDir(), "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err = restored.SearchPinned(ctx, SearchQuery{Text: "baseline"}, freshSearch.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("search cursor survived restore epoch", err)
	}
	if _, err = restored.ListEventsPinned(ctx, "session-1", 0, 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cursor survived restore epoch", err)
	}
}
