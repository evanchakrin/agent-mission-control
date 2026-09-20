package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestIndexedExportRequiresStableBoundaryAndCompleteTrailer(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	r, err := s.BeginRebuild(ctx, "session-1", "export-pages", "3")
	if err != nil {
		t.Fatal(err)
	}
	b := batch(testSource(), 0, 13)
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	b.Events = nil
	b.Usage = nil
	for i := 0; i < 250; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("export-event-%d", i), SourceLength: 13})
		b.Usage = append(b.Usage, UsageObservation{ID: fmt.Sprintf("export-usage-%d", i), TokensIn: 1})
	}
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := s.WriteIndexedExport(ctx, "session-1", &output); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(&output)
	var records []exportRecord
	for scanner.Scan() {
		var record exportRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) < 3 || records[0].Type != "header" || records[len(records)-1].Type != "complete" || records[0].Boundary != records[len(records)-1].Boundary {
		t.Fatal("export lacks matching completion evidence")
	}
	var events, usage int64
	for _, record := range records {
		if record.Type == "event" {
			events++
		}
		if record.Type == "usage" {
			usage++
		}
	}
	if events != records[len(records)-1].Events || usage != records[len(records)-1].Observations {
		t.Fatal("trailer counts differ")
	}
	if events != 250 || usage != 250 {
		t.Fatal("export failed to traverse every event/usage page")
	}
}

type changingExportWriter struct {
	bytes.Buffer
	change  func()
	changed bool
}

func (w *changingExportWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if !w.changed {
		w.changed = true
		w.change()
	}
	return n, err
}

func TestIndexedExportDoesNotCompleteAfterOrganizationChanges(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	w := &changingExportWriter{change: func() {
		yes := true
		if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "during-export", Archived: &yes}); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.WriteIndexedExport(ctx, "session-1", w); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("export crossed revision: %v", err)
	}
	if bytes.Contains(w.Bytes(), []byte(`"type":"complete"`)) {
		t.Fatal("changed export marked complete")
	}
}

func TestIndexedExportDoesNotCompleteAfterMachineLabelChanges(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	session, err := s.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	w := &changingExportWriter{change: func() {
		if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: session.MachineID, DisplayName: "Changed during export", OperationID: "export-label"}); err != nil {
			t.Fatal(err)
		}
	}}
	if err = s.WriteIndexedExport(ctx, "session-1", w); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("export ignored label change", err)
	}
	if bytes.Contains(w.Bytes(), []byte(`"type":"complete"`)) {
		t.Fatal("renamed export marked complete")
	}
	var stable bytes.Buffer
	if err = s.WriteIndexedExport(ctx, "session-1", &stable); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stable.Bytes(), []byte(`"machineName":"Changed during export"`)) || !bytes.Contains(stable.Bytes(), []byte(`"type":"complete"`)) {
		t.Fatal("stable export missing label or trailer")
	}
}

func TestExportBoundaryTracksDisplayNameNotHeartbeatProgress(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	session, err := s.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	h := protocol.Heartbeat{MachineID: session.MachineID, Name: "Reported host"}
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	before, err := s.exportBoundary(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.CapturedBytes = 100
	h.UploadedBytes = 50
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	current, err := s.exportBoundary(ctx, session.ID)
	if err != nil || current != before {
		t.Fatal("heartbeat progress invalidated export", err)
	}
	h.Name = "New reported host"
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	current, err = s.exportBoundary(ctx, session.ID)
	if err != nil || current == before {
		t.Fatal("display change not tracked", err)
	}
}

func TestIndexedExportDoesNotCompleteAfterSourceAppend(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	w := &changingExportWriter{change: func() { ingest(t, s, testSource(), 13, "appended evidence\n") }}
	if err := s.WriteIndexedExport(context.Background(), "session-1", w); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("export crossed source append: %v", err)
	}
	if bytes.Contains(w.Bytes(), []byte(`"type":"complete"`)) {
		t.Fatal("appended export marked complete")
	}
}

type failedExportWriter struct{}

func (failedExportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestIndexedExportPropagatesCancellationAndWriterFailure(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	if err := s.WriteIndexedExport(context.Background(), "session-1", failedExportWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.WriteIndexedExport(ctx, "session-1", io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
