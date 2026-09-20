package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestToolWorkingDirectoryComesFromThatRecordOnly(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "claude"}
	state, _ := DecodeState(nil)
	for i, cwd := range []any{"C:/first", nil, "C:/second", strings.Repeat("x", 4097), "bad\x00path", 42} {
		line := map[string]any{"type": "assistant", "uuid": fmt.Sprint(i), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprint(i), "name": "Edit", "input": map[string]any{"file_path": "relative.go"}}}}}
		if cwd != nil {
			line["cwd"] = cwd
		}
		raw, _ := json.Marshal(line)
		result := Record(source, &state, int64(i*10000), raw)
		if len(result.Events) != 1 {
			t.Fatal(result)
		}
		var data map[string]any
		if err := json.Unmarshal(result.Events[0].Data, &data); err != nil {
			t.Fatal(err)
		}
		want := ""
		if i == 0 || i == 2 {
			want = cwd.(string)
		}
		got, _ := data["workingDirectory"].(string)
		if got != want {
			t.Fatalf("record %d inherited, truncated or lost cwd: got %q want %q", i, got, want)
		}
		if result.Events[0].SourceLength != int64(len(raw)) {
			t.Fatal("lost complete raw record reference")
		}
	}
	if _, err := DecodeState([]byte(`{"version":"6"}`)); !errors.Is(err, ErrRebuildRequired) {
		t.Fatal("old parser accepted missing per-record context", err)
	}
}
