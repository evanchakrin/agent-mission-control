package parser

import (
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"strings"
	"testing"
)

func TestToolImageSearchPreservesEvidenceAndText(t *testing.T) {
	output := []any{map[string]any{"type": "input_text", "text": "firstneedle"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("encodedimage", 600000), "detail": "original"}, map[string]any{"type": "input_text", "text": "lastneedle"}}
	raw, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "call-1", "output": output}})
	if len(raw) > MaxRecordBytes {
		t.Fatal("fixture must be a supported record")
	}
	state, _ := DecodeState(nil)
	src := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex"}
	result := Record(src, &state, 123, raw)
	if len(result.Events) != 1 {
		t.Fatal("missing tool result")
	}
	e := result.Events[0]
	original := string(marshal(output))
	if e.Text != preview(original) || e.DedupeKey != ID("main", "custom_tool_call_output", "call-1", original) {
		t.Fatal("changed existing preview or dedupe identity")
	}
	if e.SourceOffset != 123 || e.SourceLength != int64(len(raw)) {
		t.Fatal("lost raw source binding")
	}
	if len(e.SearchText) > 1024 || !strings.Contains(e.SearchText, "firstneedle") || !strings.Contains(e.SearchText, "lastneedle") || strings.Contains(e.SearchText, "encodedimage") {
		t.Fatal("image encoding reached search or text was lost")
	}
	if !strings.Contains(output[1].(map[string]any)["image_url"].(string), "encodedimage") {
		t.Fatal("input mutated")
	}
}

func TestToolImageSearchDoesNotStripTextOrUnknownStructures(t *testing.T) {
	for _, output := range []any{"literal data:image/png;base64,abc", map[string]any{"data": "keep"}, []any{map[string]any{"type": "input_text", "text": "data:image/png;base64,keep"}}, []any{map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"}}, []any{map[string]any{"type": "unknown", "image_url": "data:image/png;base64,keep"}}} {
		original := string(marshal(output))
		if got := toolOutputSearch(output, original); got != original {
			t.Fatal("unrecognized or textual payload altered")
		}
	}
}
