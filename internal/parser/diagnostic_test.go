package parser

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// Owner-selected source, read only. Emits identities and counts, never chat text.
func TestReadOnlyCodexIdentityDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_CODEX_DIAGNOSTIC_SOURCE")
	if path == "" {
		t.Skip("no source selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute source path required")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 32<<20 {
		t.Fatal("diagnostic limited to 32 MiB sources")
	}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), MaxRecordBytes)
	state, _ := DecodeState(nil)
	source := protocol.Source{MachineID: "diagnostic", SourceID: "source", Generation: "generation", Provider: "codex"}
	var offset, tokens, unknown, conflicts int64
	for scan.Scan() {
		line := scan.Bytes()
		r := Record(source, &state, offset, line)
		offset += int64(len(line)) + 1
		for _, u := range r.Usage {
			n := u.TokensIn + u.TokensCache + u.TokensCacheWrite + u.TokensOut
			tokens += n
			if u.Kind == "incomplete-attribution" {
				unknown += n
			}
		}
		for _, e := range r.Events {
			if e.Kind == "indexing-error" {
				conflicts++
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("owner=%s parent=%s fork=%s counterScopes=%d recordedTokens=%d incompleteAttribution=%d diagnostics=%d", state.NativeID, state.ParentThreadID, state.ForkedFromID, len(state.Counters), tokens, unknown, conflicts)
}
