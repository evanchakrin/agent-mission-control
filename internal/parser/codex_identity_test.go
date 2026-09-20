package parser

import (
	"encoding/json"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestCodexCopiedParentHeaderCannotReplaceChildOrRecountCounter(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", NativeID: "filename-hint"}
	state, _ := DecodeState(nil)
	Record(source, &state, 0, []byte(`{"type":"session_meta","payload":{"id":"child","cwd":"child-project","model":"child-model","parent_thread_id":"parent","forked_from_id":"parent"}}`))
	counter := []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":20}}}}`)
	first := Record(source, &state, 100, counter)
	if len(first.Usage) != 1 {
		t.Fatalf("initial observation: %+v", first)
	}
	// Exercise the persisted checkpoint boundary used by parser children.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	state, err = DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := Record(source, &state, 200, []byte(`{"type":"session_meta","payload":{"id":"parent","cwd":"parent-project","model":"parent-model"}}`))
	if state.NativeID != "child" || state.ParentThreadID != "parent" || state.ForkedFromID != "parent" || state.Project != "child-project" || state.Model != "child-model" {
		t.Fatalf("copied metadata changed owner: %+v", state)
	}
	if len(r.Events) != 2 || r.Events[0].Kind != "session-meta" || r.Events[1].Kind != "indexing-error" {
		t.Fatalf("missing preserved conflict: %+v", r)
	}
	if repeated := Record(source, &state, 300, counter); len(repeated.Usage) != 0 {
		t.Fatalf("same cumulative counter billed again: %+v", repeated.Usage)
	}
	Record(source, &state, 400, []byte(`{"type":"session_meta","payload":{"id":"child"}}`))
	if state.ParentThreadID != "parent" || state.ForkedFromID != "parent" || state.Project != "child-project" {
		t.Fatal("partial owner header erased metadata")
	}
}

func TestCodexFirstHeaderOverridesFilenameHint(t *testing.T) {
	for _, hint := range []string{"", "renamed-file"} {
		source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", NativeID: hint}
		state, _ := DecodeState(nil)
		Record(source, &state, 0, []byte(`{"type":"session_meta","payload":{}}`))
		Record(source, &state, 100, []byte(`{"type":"session_meta","payload":{"id":"owner"}}`))
		if state.NativeID != "owner" || !state.CodexIdentitySet {
			t.Fatalf("header not established: %+v", state)
		}
	}
}
