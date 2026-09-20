// amc-fixture generates isolated synthetic source evidence, never user history.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

type fileEvidence struct {
	NativeID          string `json:"nativeId"`
	AgentID           string `json:"agentId,omitempty"`
	Provider          string `json:"provider"`
	ParentNativeID    string `json:"parentNativeId,omitempty"`
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	Bytes             int64  `json:"bytes"`
	UsageObservations int64  `json:"usageObservations"`
	RecordedTokens    int64  `json:"recordedTokens"`
}
type report struct {
	Complete       bool   `json:"complete"`
	Sessions       int64  `json:"sessions"`
	Bytes          int64  `json:"bytes"`
	RecordedTokens int64  `json:"recordedTokens"`
	Provider       string `json:"provider"`
	Relationships  string `json:"relationships,omitempty"`
}
type spaceCheck func(string) (uint64, uint64, error)

const (
	// A personal AMC installation does not need filesystem-scale certification
	// corpora. Keep the generator physically bounded so one invocation can
	// never create the hundreds of thousands of files that previously filled
	// the development drive. Large query tests use rows in temporary SQLite
	// databases instead of source files.
	maxFixtureSessions = int64(1000)
	maxFixtureBytes    = int64(256 << 20)
)

func preflight(parent string, sessions, bytes int64, space spaceCheck) error {
	if sessions < 1 || sessions > maxFixtureSessions || bytes < sessions*1024 || bytes > maxFixtureBytes {
		return fmt.Errorf("require 1..%d sessions and 1 KiB/session..%d MiB source bytes", maxFixtureSessions, maxFixtureBytes>>20)
	}
	available, total, err := space(parent)
	if err != nil {
		return err
	}
	// Each file may exceed its requested size by one <=16 KiB complete record.
	upper := uint64(bytes) + uint64(sessions)*((16<<10)+1)
	reserve := max(uint64(5<<30), total/20)
	required := 5*upper + reserve
	if available < required {
		return fmt.Errorf("fixture disk budget insufficient: available %d bytes, required %d (five-copy expansion budget plus %d reserve)", available, required, reserve)
	}
	return nil
}

func generate(ctx context.Context, directory string, sessions, bytes int64, space spaceCheck) (report, error) {
	return generateProviders(ctx, directory, sessions, bytes, space, "claude")
}

func generateProviders(ctx context.Context, directory string, sessions, bytes int64, space spaceCheck, mode string) (report, error) {
	result := report{Provider: mode, Relationships: "provider-pairs-v2"}
	if mode != "claude" && mode != "codex" && mode != "mixed" {
		return result, errors.New("provider must be claude, codex or mixed")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return result, err
	}
	parent := filepath.Dir(directory)
	if _, err = os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("output must be a new directory; existing evidence is never overwritten")
	}
	if err = preflight(parent, sessions, bytes, space); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = os.Mkdir(directory, 0700); err != nil {
		return result, err
	}
	providers := []string{mode}
	if mode == "mixed" {
		providers = []string{"claude", "codex"}
	}
	for _, provider := range providers {
		if err = os.Mkdir(filepath.Join(directory, provider), 0700); err != nil {
			return result, err
		}
	}
	// Incomplete output stays intact after failure; only summary.json proves completion.
	manifest, err := os.OpenFile(filepath.Join(directory, "files.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer manifest.Close()
	encoder := json.NewEncoder(manifest)
	target := (bytes + sessions - 1) / sessions
	for i := int64(0); i < sessions; i++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		available, total, e := space(directory)
		if e != nil {
			return result, e
		}
		if available < max(uint64(5<<30), total/20)+uint64(target)+(16<<10) {
			return result, errors.New("storage reserve reached during generation; partial evidence retained")
		}
		provider := mode
		if mode == "mixed" {
			provider = "claude"
			if i%2 == 1 {
				provider = "codex"
			}
		}
		root := provider
		if provider == "codex" && i%4 == 3 {
			root = filepath.Join(root, "archived_sessions")
		}
		relative := filepath.Join(root, fmt.Sprintf("shard-%04d", i/1000), fmt.Sprintf("fixture-%012d.jsonl", i))
		path := filepath.Join(directory, relative)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return result, err
		}
		evidence, e := writeSession(ctx, path, i, target, space, provider)
		if e != nil {
			return result, e
		}
		evidence.Path = filepath.ToSlash(relative)
		if err = encoder.Encode(evidence); err != nil {
			return result, err
		}
		result.Sessions++
		result.Bytes += evidence.Bytes
		result.RecordedTokens += evidence.RecordedTokens
	}
	if err = manifest.Sync(); err != nil {
		return result, err
	}
	result.Complete = true
	summary, err := os.OpenFile(filepath.Join(directory, "summary.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer summary.Close()
	if err = json.NewEncoder(summary).Encode(result); err != nil {
		return result, err
	}
	return result, summary.Sync()
}

func writeSession(ctx context.Context, path string, index, target int64, space spaceCheck, provider string) (fileEvidence, error) {
	result := fileEvidence{Provider: provider, NativeID: fmt.Sprintf("amc-fixture-%012d", index)}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer f.Close()
	hash := sha256.New()
	writer := io.MultiWriter(f, hash)
	header := fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"amc-fixture-%012d\",\"uuid\":\"user-0\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"AMC synthetic acceptance session %d\"}}\n", index, index)
	if provider == "codex" {
		header = fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"payload\":{\"id\":\"amc-fixture-%012d\",\"model\":\"amc-fixture-model\",\"thread_source\":\"user\"}}\n", index)
	}
	if index%4 >= 2 {
		result.ParentNativeID = fmt.Sprintf("amc-fixture-%012d", index-2)
		if provider == "codex" {
			header = strings.Replace(header, "\"model\":", fmt.Sprintf("\"parent_thread_id\":\"%s\",\"model\":", result.ParentNativeID), 1)
		} else {
			result.AgentID = fmt.Sprintf("amc-fixture-agent-%012d", index)
			identity := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(result.ParentNativeID), result.ParentNativeID, len(result.AgentID), result.AgentID)))
			result.NativeID = "claude-agent:" + hex.EncodeToString(identity[:])
			header = strings.Replace(header, fmt.Sprintf("amc-fixture-%012d", index), result.ParentNativeID, 1)
			header = strings.Replace(header, "\"type\":", fmt.Sprintf("\"agentId\":%q,\"isSidechain\":true,\"type\":", result.AgentID), 1)
		}
	}
	n, err := io.WriteString(writer, header)
	result.Bytes = int64(n)
	if err != nil {
		return result, err
	}
	padding := strings.Repeat("synthetic transcript evidence ", 400)
	var lastSpaceCheck int64
	for record := int64(0); result.Bytes < target; record++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if result.Bytes-lastSpaceCheck >= 16<<20 {
			available, total, e := space(filepath.Dir(path))
			if e != nil {
				return result, e
			}
			if available < max(uint64(5<<30), total/20)+(16<<20) {
				return result, errors.New("storage reserve reached during transcript generation; partial evidence retained")
			}
			lastSpaceCheck = result.Bytes
		}
		line := fmt.Sprintf("{\"type\":\"assistant\",\"uuid\":\"assistant-%d\",\"parentUuid\":\"user-0\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"message\":{\"id\":\"message-%d\",\"role\":\"assistant\",\"model\":\"amc-fixture-model\",\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":3,\"output_tokens\":4},\"content\":[{\"type\":\"text\",\"text\":\"record %d %s\"}]}}\n", record, record, record, padding)
		if result.AgentID != "" {
			line = strings.Replace(line, "\"type\":", fmt.Sprintf("\"sessionId\":%q,\"agentId\":%q,\"isSidechain\":true,\"type\":", result.ParentNativeID, result.AgentID), 1)
		}
		if provider == "codex" {
			line = fmt.Sprintf("{\"type\":\"event_msg\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"payload\":{\"type\":\"agent_message\",\"message\":\"record %d %s\"}}\n{\"type\":\"event_msg\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":%d,\"cached_input_tokens\":%d,\"output_tokens\":%d,\"total_tokens\":%d}}}}\n", record, padding, (record+1)*7, (record+1)*3, (record+1)*3, (record+1)*10)
		}
		if len(line) > 16<<10 {
			return result, errors.New("fixture record exceeded preflight bound")
		}
		n, err = io.WriteString(writer, line)
		result.Bytes += int64(n)
		if err != nil {
			return result, err
		}
		result.UsageObservations++
		result.RecordedTokens += 10
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, f.Sync()
}

func main() {
	output := flag.String("output", "", "new isolated output directory (required)")
	sessions := flag.Int64("sessions", 100, "synthetic sessions (hard limit 1000)")
	bytes := flag.Int64("bytes", 32<<20, "minimum complete source bytes (hard limit 256 MiB)")
	provider := flag.String("provider", "mixed", "claude, codex, or mixed (alternating providers, including archived Codex sources)")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	result, err := generateProviders(ctx, *output, *sessions, *bytes, platform.FreeSpace, *provider)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
