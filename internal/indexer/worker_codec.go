package indexer

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

const MaxWorkerRequestBytes = 64 << 10
const MaxWorkerResponseBytes = 32 << 20

// WorkerDecoder reads one JSON line at a time without allowing an unbounded
// child response to exhaust the hub. An oversized frame is a terminal IPC error;
// prepared work is not acknowledged as indexed. For legacy committing calls,
// callers must reread the durable checkpoint after an uncertain response.
type WorkerDecoder struct {
	scanner *bufio.Scanner
	limit   int
}

func NewWorkerDecoder(r io.Reader, limit int) *WorkerDecoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, min(64<<10, limit+1)), limit+1)
	return &WorkerDecoder{scanner: s, limit: limit}
}

func (d *WorkerDecoder) Decode(v any) error {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	if len(d.scanner.Bytes()) > d.limit {
		return errors.New("parser IPC frame exceeds budget")
	}
	return json.Unmarshal(d.scanner.Bytes(), v)
}

// WriteWorkerResult checks the wire budget before writing any of the response.
// An oversized preparation becomes an explicit error, not published progress.
func WriteWorkerResult(w io.Writer, result WorkResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(data) > MaxWorkerResponseBytes {
		data, err = json.Marshal(WorkResult{Error: "parser response exceeds IPC budget; reread durable checkpoint before retry"})
		if err != nil {
			return err
		}
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (r WorkRequest) ValidatePreparation() error {
	if r.Revision != "" && r.SourceID == "" && r.Generation == "" && len(r.Sources) == 0 {
		return nil
	}
	if r.SourceID == "" || r.Generation == "" || len(r.Sources) != 0 || r.Revision != "" {
		return errors.New("preparation requires exactly one source and generation")
	}
	return nil
}
