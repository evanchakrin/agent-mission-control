package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func ample(string) (uint64, uint64, error) { return 100 << 30, 120 << 30, nil }

func TestGeneratorEvidenceMatchesParserAndChecksums(t *testing.T) {
	for _, mode := range []string{"claude", "codex", "mixed"} {
		t.Run(mode, func(t *testing.T) { verifyGeneratedEvidence(t, mode) })
	}
}

func verifyGeneratedEvidence(t *testing.T, mode string) {
	dir := filepath.Join(t.TempDir(), "corpus")
	report, err := generateProviders(context.Background(), dir, 4, 128<<10, ample, mode)
	if err != nil || !report.Complete || report.Sessions != 4 || report.Bytes < 128<<10 {
		t.Fatalf("%+v %v", report, err)
	}
	manifest, err := os.Open(filepath.Join(dir, "files.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	scanner := bufio.NewScanner(manifest)
	var tokens, bytes int64
	var files int
	for scanner.Scan() {
		var evidence fileEvidence
		if err = json.Unmarshal(scanner.Bytes(), &evidence); err != nil {
			t.Fatal(err)
		}
		expectedParent := ""
		if files%4 >= 2 {
			expectedParent = fmt.Sprintf("amc-fixture-%012d", files-2)
		}
		if evidence.ParentNativeID != expectedParent {
			t.Fatalf("fixture relationship pattern differs: %s/%s", evidence.ParentNativeID, expectedParent)
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(evidence.Path)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != evidence.SHA256 || int64(len(raw)) != evidence.Bytes {
			t.Fatal("manifest mismatch")
		}
		state, err := parser.DecodeState(nil)
		if err != nil {
			t.Fatal(err)
		}
		source := protocol.Source{Provider: evidence.Provider, MachineID: "fixture", SourceID: evidence.Path, Generation: "fixture"}
		var offset, observations, recorded int64
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(evidence.Path)))
		if err != nil {
			t.Fatal(err)
		}
		records := bufio.NewScanner(f)
		for records.Scan() {
			parsed := parser.Record(source, &state, offset, records.Bytes())
			offset += int64(len(records.Bytes()) + 1)
			for _, u := range parsed.Usage {
				if evidence.Provider == "codex" {
					if u.TokensIn != 4 || u.TokensCache != 3 || u.TokensCacheWrite != 0 || u.TokensOut != 3 {
						t.Fatalf("Codex cumulative counter split differs: %+v", u)
					}
				} else if u.TokensIn != 1 || u.TokensCache != 2 || u.TokensCacheWrite != 3 || u.TokensOut != 4 {
					t.Fatalf("Claude usage split differs: %+v", u)
				}
				observations++
				recorded += u.TokensIn + u.TokensCache + u.TokensCacheWrite + u.TokensOut
			}
		}
		if err = records.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if state.ParentThreadID != evidence.ParentNativeID {
			t.Fatalf("parent relationship differs: %s/%s", state.ParentThreadID, evidence.ParentNativeID)
		}
		if state.NativeID != evidence.NativeID || state.ClaudeAgentID != evidence.AgentID {
			t.Fatalf("generated identity differs: %+v %+v", state, evidence)
		}
		if evidence.AgentID != "" {
			var first map[string]any
			if err = json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &first); err != nil {
				t.Fatal(err)
			}
			if first["sessionId"] != evidence.ParentNativeID || first["agentId"] != evidence.AgentID || first["isSidechain"] != true || first["parentSessionId"] != nil {
				t.Fatal("Claude fixture does not use the observed sidechain shape")
			}
		}
		if observations != evidence.UsageObservations || recorded != evidence.RecordedTokens {
			t.Fatalf("parser differs: observations %d/%d tokens %d/%d", observations, evidence.UsageObservations, recorded, evidence.RecordedTokens)
		}
		tokens += recorded
		bytes += evidence.Bytes
		files++
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if files != 4 || tokens != report.RecordedTokens || bytes != report.Bytes {
		t.Fatal("summary does not reconcile")
	}
	if _, err = generate(context.Background(), dir, 3, 128<<10, ample); err == nil {
		t.Fatal("overwrote existing corpus")
	}
}

func TestGeneratorRefusesInsufficientDiskBeforeCreatingOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	_, err := generate(context.Background(), dir, 100001, 20<<30, func(string) (uint64, uint64, error) { return 100 << 30, 1 << 40, nil })
	if err == nil {
		t.Fatal("accepted insufficient expansion budget")
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("preflight created output")
	}
}

func TestGeneratorHardCapsFilesystemCorpusBeforeCreatingOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions int64
		bytes    int64
	}{
		"too-many-files": {maxFixtureSessions + 1, (maxFixtureSessions + 1) * 1024},
		"too-many-bytes": {1, maxFixtureBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "must-not-exist")
			_, err := generate(context.Background(), dir, tc.sessions, tc.bytes, ample)
			if err == nil {
				t.Fatal("accepted a filesystem corpus above the personal-use hard cap")
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("rejected corpus created output: %v", statErr)
			}
		})
	}
}

func TestGeneratorCancellationRetainsIncompleteEvidence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "partial")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := generate(ctx, dir, 10, 128<<10, func(path string) (uint64, uint64, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return ample(path)
	})
	if err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err = os.Stat(filepath.Join(dir, "summary.json")); !os.IsNotExist(err) {
		t.Fatal("incomplete corpus has completion marker")
	}
	if _, err = os.Stat(filepath.Join(dir, "files.jsonl")); err != nil {
		t.Fatal("partial evidence removed")
	}
}

func TestGeneratorChecksReserveWithinLargeTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "partial-large")
	calls := 0
	_, err := generate(context.Background(), dir, 1, 20<<20, func(path string) (uint64, uint64, error) {
		calls++
		if calls >= 3 {
			return 5 << 30, 120 << 30, nil
		}
		return ample(path)
	})
	if err == nil || calls < 3 {
		t.Fatal("large transcript did not recheck reserve")
	}
	if _, err = os.Stat(filepath.Join(dir, "summary.json")); !os.IsNotExist(err) {
		t.Fatal("reserve failure marked complete")
	}
	info, err := os.Stat(filepath.Join(dir, "claude", "shard-0000", "fixture-000000000000.jsonl"))
	if err != nil || info.Size() > 17<<20 {
		t.Fatalf("reserve check did not bound ongoing writes: %v %v", info, err)
	}
}
