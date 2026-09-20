package indexer

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestPreparationWirePreservesPrivateSearchEvidence(t *testing.T) {
	p := Preparation{Records: 1, Batch: &store.IndexBatch{Events: []store.Event{{Text: "preview", SearchText: "complete search evidence"}}}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Preparation
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Batch.Events[0].SearchText != p.Batch.Events[0].SearchText {
		t.Fatal("search evidence lost")
	}
	public, err := json.Marshal(decoded.Batch.Events[0])
	if err != nil || bytes.Contains(public, []byte("complete search evidence")) {
		t.Fatal("private evidence leaked into public event", err)
	}
	data = bytes.Replace(data, []byte(`"searchText":["complete search evidence"]`), []byte(`"searchText":[]`), 1)
	if err = json.Unmarshal(data, &decoded); err == nil {
		t.Fatal("missing search evidence accepted")
	}
}

func TestWorkerDecoderBoundaries(t *testing.T) {
	for _, newline := range []string{"", "\n"} {
		for _, size := range []int{15, 16, 17, 100} {
			input := `"` + strings.Repeat("x", size-2) + `"` + newline
			var value string
			err := NewWorkerDecoder(strings.NewReader(input), 16).Decode(&value)
			if (err == nil) != (size <= 16) {
				t.Fatalf("size=%d newline=%q: %v", size, newline, err)
			}
		}
	}
	d := NewWorkerDecoder(strings.NewReader("{\"records\":1}\n{\"records\":2}\n"), 64)
	for _, want := range []int64{1, 2} {
		var got WorkResult
		if err := d.Decode(&got); err != nil || got.Records != want {
			t.Fatal(got, err)
		}
	}
	if err := d.Decode(&WorkResult{}); err != io.EOF {
		t.Fatal(err)
	}
	if err := NewWorkerDecoder(strings.NewReader("{} {}\n"), 64).Decode(&WorkResult{}); err == nil {
		t.Fatal("multiple values accepted")
	}
}

func TestWorkerResultBudgetAndPreparation(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteWorkerResult(&wire, WorkResult{Prepared: &Preparation{Records: 3}}); err != nil {
		t.Fatal(err)
	}
	var result WorkResult
	if err := NewWorkerDecoder(&wire, MaxWorkerResponseBytes).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Records != 0 || result.Prepared == nil || result.Prepared.Records != 3 {
		t.Fatal(result)
	}
	if err := WriteWorkerResult(&wire, WorkResult{Error: strings.Repeat("x", MaxWorkerResponseBytes+1)}); err != nil {
		t.Fatal(err)
	}
	result = WorkResult{}
	if err := NewWorkerDecoder(&wire, MaxWorkerResponseBytes).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Error == "" || result.Records != 0 || result.Prepared != nil {
		t.Fatal("oversize response claimed progress")
	}
}
