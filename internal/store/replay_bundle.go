package store

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
)

// WriteReplayBundle streams one immutable export boundary. No transcript-sized
// buffers or temporary source copies. Store compression avoids expensive CPU
// work on already large histories; Zip64 handles large entries automatically.
// An interrupted/changed export has no completed ZIP central directory.
func (s *Store) WriteReplayBundle(ctx context.Context, id string, viewer []byte, w io.Writer) error {
	boundary, err := s.exportBoundary(ctx, id)
	if err != nil {
		return err
	}
	row, err := s.GetSession(ctx, id)
	if err != nil {
		return err
	}
	source, err := s.SourceState(ctx, row.SourceID, row.Generation)
	if err != nil {
		return err
	}
	if len(viewer) == 0 || len(viewer) > 1<<20 {
		return ErrInvalid
	}
	archive := zip.NewWriter(w)
	entry := func(name string) (io.Writer, error) {
		return archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	}
	file, err := entry("replay.html")
	if err != nil {
		return err
	}
	if _, err = file.Write(viewer); err != nil {
		return err
	}
	file, err = entry("indexed-history.jsonl")
	if err != nil {
		return err
	}
	if err = s.WriteIndexedExport(ctx, id, file); err != nil {
		return err
	}
	file, err = entry("complete-source.jsonl")
	if err != nil {
		return err
	}
	raw, err := s.OpenSource(ctx, row.SourceID, row.Generation, 0)
	if err != nil {
		return err
	}
	defer raw.Close()
	copied, err := io.CopyBuffer(contextWriter{ctx: ctx, w: file}, io.LimitReader(raw, source.DurableOffset), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if copied != source.DurableOffset {
		return io.ErrUnexpectedEOF
	}
	// Compare again after raw copying so no mixture of organizations, prices,
	// generations, or source/index checkpoints can be published as complete.
	current, err := s.exportBoundary(ctx, id)
	if err != nil {
		return err
	}
	if current != boundary {
		return ErrHistoryChanged
	}
	file, err = entry("README.txt")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "AMC offline replay package\n\nExtract all files. Open replay.html and select indexed-history.jsonl.\nThe viewer works offline and never uploads your files. Indexed text may be abbreviated; complete-source.jsonl preserves all %d captured source bytes at this export boundary. Capture itself may be incomplete.\n\nThese files contain private chat history. Share deliberately. Recorded usage is not a provider invoice. A valid ZIP central directory and the indexed export's complete trailer are required; validation does not establish authenticity.\n\nBoundary: %s\n", source.DurableOffset, boundary)
	if err != nil {
		return err
	}
	return archive.Close()
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
