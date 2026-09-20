package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestClaudeChildClassificationDoesNotPromoteUnprovenAgentIDs(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	src := protocol.Source{MachineID: "m", SourceID: "child", Generation: "g", GenerationSequence: 1, Provider: "claude"}
	data := []byte("{\"type\":\"assistant\",\"sessionId\":\"parent\",\"agentId\":\"known\",\"isSidechain\":true,\"message\":{\"id\":\"one\",\"usage\":{\"input_tokens\":10}}}\n{\"type\":\"assistant\",\"agentId\":\"unproven\",\"message\":{\"id\":\"two\",\"usage\":{\"input_tokens\":5}}}\n")
	src.Size = int64(len(data))
	sum := sha256.Sum256(data)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	page, err := s.SessionAgents(ctx, parser.SessionID(src), "", 100)
	if err != nil || len(page.Agents) != 2 {
		t.Fatalf("agent evidence omitted: %+v %v", page, err)
	}
	for _, agent := range page.Agents {
		if agent.ID == "known" {
			if agent.Kind != "child-agent" || agent.TokensIn != 10 {
				t.Fatalf("known agent lost: %+v", agent)
			}
		} else if agent.ID != "unproven" || agent.Kind != "unclassified" || agent.TokensIn != 5 {
			t.Fatalf("unproven identity promoted: %+v", agent)
		}
	}
	groups, err := s.GroupedUsage(ctx, store.GroupQuery{Dimension: "agentKind"})
	if err != nil || len(groups.Groups) != 2 {
		t.Fatalf("unproven group lost: %+v %v", groups, err)
	}
	for _, group := range groups.Groups {
		if group.Key == "child-agent" {
			if group.TokensIn != 10 {
				t.Fatal(group)
			}
		} else if group.Key != "unclassified" || group.TokensIn != 5 {
			t.Fatal(group)
		}
	}
}

func TestClaudeSiblingRebuildResolvesContainingSessionAndPreservesOrganization(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	ids := map[string]string{}
	for _, agent := range []string{"root", "child-a", "child-b"} {
		src := protocol.Source{MachineID: "machine", SourceID: agent, Generation: "g", GenerationSequence: 1, Provider: "claude"}
		extra := ""
		if agent != "root" {
			extra = fmt.Sprintf(`,"agentId":%q,"isSidechain":true`, agent)
		}
		data := []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"parent-session\"%s,\"message\":{\"content\":\"Sanitized lineage fixture\"}}\n{\"type\":\"assistant\",\"sessionId\":\"parent-session\"%s,\"message\":{\"id\":\"message\",\"model\":\"fixture-model\",\"content\":\"Response\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n", extra, extra))
		src.Size = int64(len(data))
		sum := sha256.Sum256(data)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		id := parser.SessionID(src)
		ids[agent] = id
		if agent == "root" {
			if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
				t.Fatal(err)
			}
			continue
		}
		// Reconstruct v2's shared native alias, but keep durable raw bytes exact.
		old := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, ToOffset: src.Size, ParserState: json.RawMessage(`{"version":"2","nativeId":"parent-session"}`), Session: store.Session{ID: id, NativeID: "parent-session"}, Usage: []store.UsageObservation{{ID: "old-" + agent, AgentID: agent, Model: "fixture-model", TokensIn: 10, TokensOut: 3, Kind: "message-final"}}}
		if err = s.CommitIndex(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	name := "Owner child name"
	metadata, err := s.PatchMetadata(ctx, ids["child-a"], store.MetadataPatch{OperationID: "archive-child", Archived: &yes, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"child-a", "child-b"} {
		job, err := s.BeginRebuild(ctx, ids[agent], "identity-"+agent, parser.Version)
		if err != nil {
			t.Fatal(err)
		}
		for step := 0; step < 10; step++ {
			if _, err = x.RebuildOnce(ctx, job.Revision); err != nil {
				t.Fatal(err)
			}
			state, e := s.ProjectionRevision(ctx, job.Revision)
			if e != nil {
				t.Fatal(e)
			}
			if state.State == "active" {
				break
			}
			if step == 9 {
				t.Fatal("identity rebuild failed to publish")
			}
		}
		if agent == "child-a" {
			lineage, e := s.SessionLineage(ctx, ids[agent], "", 100)
			if e != nil || lineage.ParentResolution != "ambiguous" {
				t.Fatalf("unrebuilt sibling's ambiguous alias was hidden: %+v %v", lineage, e)
			}
		}
	}
	root, err := s.SessionLineage(ctx, ids["root"], "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if root.ParentResolution != "root" || len(root.Children) != 1 || root.NextCursor == "" {
		t.Fatalf("root child pagination missing: %+v", root)
	}
	second, err := s.SessionLineage(ctx, ids["root"], root.NextCursor, 1)
	if err != nil || len(second.Children) != 1 || second.Children[0].ID == root.Children[0].ID || second.NextCursor != "" {
		t.Fatalf("sibling omitted or repeated: %+v %v", second, err)
	}
	for _, agent := range []string{"child-a", "child-b"} {
		lineage, e := s.SessionLineage(ctx, ids[agent], "", 100)
		if e != nil || lineage.ParentResolution != "unique" || len(lineage.Parents) != 1 || lineage.Parents[0].ID != ids["root"] || len(lineage.Children) != 0 {
			t.Fatalf("wrong containing session for %s: %+v %v", agent, lineage, e)
		}
		row, e := s.GetSession(ctx, ids[agent])
		if e != nil || row.TokensIn != 10 || row.TokensOut != 3 || row.NativeID == "parent-session" {
			t.Fatalf("identity change altered tokens: %+v %v", row, e)
		}
		if agent == "child-a" && (!row.Metadata.Archived || row.Metadata.Name != name || row.Metadata.Revision != metadata.Revision) {
			t.Fatalf("rebuild changed organization: %+v", row.Metadata)
		}
		if row.NativeAgentID != agent {
			t.Fatalf("child agent provenance not published: %+v", row)
		}
		agents, e := s.SessionAgents(ctx, ids[agent], "", 100)
		if e != nil || len(agents.Agents) != 1 || agents.Agents[0].ID != agent || agents.Agents[0].Kind != "child-agent" || agents.Agents[0].TokensIn != 10 || agents.Agents[0].TokensOut != 3 {
			t.Fatalf("child accounting not classified from evidence: %+v %v", agents, e)
		}
	}
	groups, err := s.GroupedUsage(ctx, store.GroupQuery{Dimension: "agentKind"})
	if err != nil || len(groups.Groups) != 2 {
		t.Fatalf("unexpected agent categories: %+v %v", groups, err)
	}
	for _, group := range groups.Groups {
		want := int64(13)
		if group.Key == "child-agent" {
			want = 26
		} else if group.Key != "main" {
			t.Fatalf("unexpected group %s", group.Key)
		}
		if group.TokensIn+group.TokensOut != want {
			t.Fatalf("classified tokens changed: %+v", group)
		}
	}
	totals, err := s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.Sessions != 3 || totals.TokensIn != 30 || totals.TokensOut != 9 {
		t.Fatalf("rebuild duplicated history: %+v %v", totals, err)
	}
}
