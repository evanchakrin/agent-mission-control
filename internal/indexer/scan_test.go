package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestScanCheckpointBoundsRestartAndOversizeCompletion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	src := protocol.Source{MachineID: "machine", SourceID: "scan-source", Generation: "g", Provider: "claude"}
	var uploaded int64
	send := func(data []byte) {
		for len(data) > 0 {
			n := min(len(data), 1<<20)
			b := data[:n]
			h := sha256.Sum256(b)
			src.Size = uploaded + int64(n)
			_, e := s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: uploaded, Length: int64(n), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(b))
			if e != nil {
				t.Fatal(e)
			}
			uploaded += int64(n)
			data = data[n:]
		}
	}
	send(bytes.Repeat([]byte("x"), 10<<20))
	x := Indexer{Store: s}
	initial, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{4 << 20, 8 << 20, 10 << 20} {
		if n, e := x.Once(ctx, src.SourceID, src.Generation); e != nil || n != 0 {
			t.Fatal(n, e)
		}
		st, e := s.SourceState(ctx, src.SourceID, src.Generation)
		if e != nil {
			t.Fatal(e)
		}
		p, e := parser.DecodeState(st.ParserState)
		if e != nil || p.ScanOffset != want || st.IndexedOffset != 0 {
			t.Fatal("unbounded or falsely indexed scan", st, p, e)
		}
		pending, e := s.PendingParserScan(ctx, src.SourceID, src.Generation)
		if e != nil || pending != (i < 2) {
			t.Fatal("scheduler readiness", pending, e)
		}
		if i == 0 {
			if e := s.SaveParserScan(ctx, initial, st.ParserState); !errors.Is(e, store.ErrConflict) {
				t.Fatal("stale scan accepted", e)
			}
			if e := s.Close(); e != nil {
				t.Fatal(e)
			}
			s, e = store.Open(dir, store.Options{})
			if e != nil {
				t.Fatal(e)
			}
			x.Store = s
		}
	}
	if n, e := x.Once(ctx, src.SourceID, src.Generation); n != 0 || e != nil {
		t.Fatal("idle tail", n, e)
	}
	send([]byte("\n{\"type\":\"assistant\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"content\":\"ok\",\"usage\":{\"input_tokens\":7}}}\n"))
	if n, e := x.Once(ctx, src.SourceID, src.Generation); n != 1 || e != nil {
		t.Fatal("large-record completion", n, e)
	}
	if n, e := x.Once(ctx, src.SourceID, src.Generation); n != 1 || e != nil {
		t.Fatal("following record completion", n, e)
	}
	st, e := s.SourceState(ctx, src.SourceID, src.Generation)
	if e != nil || st.IndexedOffset != uploaded {
		t.Fatal(st, e)
	}
	session, e := s.GetSession(ctx, parser.SessionID(src))
	if e != nil || session.TokensIn != 7 {
		t.Fatal(session, e)
	}
	job, e := s.BeginRebuild(ctx, session.ID, "rescan", parser.Version)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if n, e := x.RebuildOnce(ctx, job.Revision); n != 0 || e != nil {
			t.Fatal(n, e)
		}
		st, e := s.ProjectionRevision(ctx, job.Revision)
		if e != nil || st.State != "building" || st.IndexedOffset != 0 {
			t.Fatal("premature rebuild verification", st, e)
		}
	}
	for i := 0; i < 12; i++ {
		if _, e := x.RebuildOnce(ctx, job.Revision); e != nil {
			t.Fatal(e)
		}
		st, e := s.ProjectionRevision(ctx, job.Revision)
		if e != nil {
			t.Fatal(e)
		}
		if st.State == "active" {
			return
		}
	}
	t.Fatal("bounded rebuild did not publish")
}

func TestSupportedRecordSpanningScanBatches(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := append([]byte(`{"type":"assistant","message":{"id":"large","model":"m","content":"`), bytes.Repeat([]byte("x"), 5<<20)...)
	raw = append(raw, []byte("\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}}\n")...)
	src := protocol.Source{MachineID: "machine", SourceID: "supported", Generation: "g", Provider: "claude", Size: int64(len(raw))}
	for offset := 0; offset < len(raw); {
		n := min(1<<20, len(raw)-offset)
		b := raw[offset : offset+n]
		h := sha256.Sum256(b)
		if _, e := s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: int64(offset), Length: int64(n), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(b)); e != nil {
			t.Fatal(e)
		}
		offset += n
	}
	x := Indexer{Store: s}
	if n, e := x.Once(ctx, src.SourceID, src.Generation); n != 0 || e != nil {
		t.Fatal(n, e)
	}
	if n, e := x.Once(ctx, src.SourceID, src.Generation); n != 1 || e != nil {
		t.Fatal(n, e)
	}
	got, e := s.GetSession(ctx, parser.SessionID(src))
	if e != nil || got.TokensIn != 17 || got.TokensOut != 3 {
		t.Fatal("resumed record lost usage", got, e)
	}
	st, e := s.SourceState(ctx, src.SourceID, src.Generation)
	if e != nil || st.IndexedOffset != int64(len(raw)) {
		t.Fatal(st, e)
	}
	p, e := parser.DecodeState(st.ParserState)
	if e != nil || p.ScanOffset != 0 {
		t.Fatal("completed scan not cleared", p, e)
	}
}
