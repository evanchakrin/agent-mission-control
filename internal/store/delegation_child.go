package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

type DelegationChild struct {
	State          string `json:"state"`
	Snapshot       string `json:"snapshot"`
	ResultSequence *int64 `json:"resultSequence,omitempty"`
	ChildSessionID string `json:"childSessionId,omitempty"`
	ChildSnapshot  string `json:"childSnapshot,omitempty"`
	ChildAgentID   string `json:"childAgentId,omitempty"`
}

// Resolve only recorded relationships. A resolved child does not imply that its
// task completed successfully. Unsupported provider output remains unavailable.
func (s *Store) ResolveDelegationChild(ctx context.Context, id string, anchor int64, expected string) (DelegationChild, error) {
	out := DelegationChild{State: "unresolved"}
	if anchor < 1 || id == "" || len(id) > 4096 || len(expected) > 128 {
		return out, ErrInvalid
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	parent, err := s.GetSession(ctx, id)
	if err != nil {
		return out, err
	}
	out.Snapshot, err = s.historySnapshot(ctx, id, expected)
	if err != nil {
		return out, err
	}
	if out.Snapshot != s.eventSnapshot(id, parent.SourceID, parent.Generation, parent.ProjectionRevision) {
		return out, ErrHistoryChanged
	}
	spans, err := s.ToolSpans(ctx, id, anchor-1, 1, out.Snapshot)
	if err != nil {
		return out, err
	}
	if len(spans.Spans) != 1 || spans.Spans[0].Call.Sequence != anchor {
		return out, ErrNotFound
	}
	if parent.Provider != "codex" && parent.Provider != "claude" {
		out.State = "unsupported-provider"
		return out, nil
	}
	span := spans.Spans[0]
	var call struct {
		Tool string `json:"tool"`
		ID   string `json:"toolUseId"`
	}
	if json.Unmarshal(span.Call.Data, &call) != nil {
		return out, ErrInvalid
	}
	supported := parent.Provider == "codex" && (call.Tool == "spawn_agent" || call.Tool == "functions.spawn_agent" || call.Tool == "collaboration.spawn_agent") || parent.Provider == "claude" && (call.Tool == "Task" || call.Tool == "Agent")
	if !supported {
		out.State = "not-supported-spawn"
		return out, nil
	}
	if parent.Completeness != "complete" && parent.Completeness != "indexed-source" {
		out.State = "parent-evidence-incomplete"
		return out, nil
	}
	if span.State != "matched" {
		out.State = "call-result-" + span.State
		return out, nil
	}
	out.ResultSequence = span.ResultSequence
	page, err := s.ListEventsPinned(ctx, id, *span.ResultSequence-1, 1, out.Snapshot)
	if err != nil {
		return out, err
	}
	if len(page.Events) != 1 || page.Events[0].Sequence != *span.ResultSequence {
		return out, ErrHistoryChanged
	}
	e := page.Events[0]
	if e.Kind != "tool-result" || e.AgentID != span.Call.AgentID {
		return out, ErrHistoryChanged
	}
	if e.SourceLength <= 0 {
		out.State = "missing-source"
		return out, nil
	}
	if e.SourceLength > 8<<20 {
		out.State = "oversize-source"
		return out, nil
	}
	raw, err := s.OpenSource(ctx, parent.SourceID, parent.Generation, e.SourceOffset)
	if err != nil {
		return out, err
	}
	body := make([]byte, int(e.SourceLength))
	_, err = io.ReadFull(raw, body)
	closeErr := raw.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}
	containing := parent.NativeID
	if parent.Provider == "claude" && parent.NativeAgentID != "" {
		containing = parent.ParentThreadID
	}
	var child, state string
	if parent.Provider == "claude" {
		child, state = claudeSpawnChild(body, call.ID, containing, span.Call.AgentID)
	} else {
		child, state = codexSpawnChild(body, call.ID)
	}
	if state != "known" {
		out.State = state
		return out, nil
	}
	if parent.NativeID == "" || containing == "" {
		out.State = "missing-parent-identity"
		return out, nil
	}
	var parentCount int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT id FROM query_sessions WHERE machine_id=? AND provider=? AND native_id=? LIMIT 2)`, parent.MachineID, parent.Provider, parent.NativeID).Scan(&parentCount); err != nil {
		return out, err
	}
	if parentCount != 1 {
		out.State = "ambiguous-parent"
		return out, nil
	}
	identity := "native_id"
	if parent.Provider == "claude" {
		identity = "native_agent_id"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,source_id,generation,projection_revision FROM query_sessions WHERE machine_id=? AND provider=? AND `+identity+`=? AND parent_native_id=? AND id<>? ORDER BY id LIMIT 2`, parent.MachineID, parent.Provider, child, containing, id)
	if err != nil {
		return out, err
	}
	n := 0
	for rows.Next() {
		var session, source, generation, revision string
		if err = rows.Scan(&session, &source, &generation, &revision); err != nil {
			rows.Close()
			return out, err
		}
		n++
		out.ChildSessionID = session
		out.ChildSnapshot = s.eventSnapshot(session, source, generation, revision)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if _, err = s.historySnapshot(ctx, id, out.Snapshot); err != nil {
		return DelegationChild{}, err
	}
	current, err := s.GetSession(ctx, id)
	if err != nil {
		return DelegationChild{}, err
	}
	if current.EventCount != parent.EventCount || current.NativeID != parent.NativeID || current.NativeAgentID != parent.NativeAgentID || current.ParentThreadID != parent.ParentThreadID || current.MachineID != parent.MachineID || current.SourceID != parent.SourceID || current.Generation != parent.Generation || current.ProjectionRevision != parent.ProjectionRevision {
		return DelegationChild{}, ErrHistoryChanged
	}
	if n != 1 {
		out.ChildSessionID = ""
		out.ChildSnapshot = ""
		out.State = "child-not-indexed-or-parent-mismatch"
		if n > 1 {
			out.State = "ambiguous-child"
		}
		return out, nil
	}
	childSession, err := s.GetSession(ctx, out.ChildSessionID)
	if err != nil {
		return DelegationChild{}, err
	}
	childIdentity := childSession.NativeID
	if parent.Provider == "claude" {
		childIdentity = childSession.NativeAgentID
	}
	if childIdentity != child || childSession.ParentThreadID != containing || childSession.MachineID != parent.MachineID || childSession.Provider != parent.Provider || s.eventSnapshot(childSession.ID, childSession.SourceID, childSession.Generation, childSession.ProjectionRevision) != out.ChildSnapshot {
		return DelegationChild{}, ErrHistoryChanged
	}
	var captured int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE source_id=? AND generation=? AND indexed_offset>0`, childSession.SourceID, childSession.Generation).Scan(&captured); err != nil {
		return DelegationChild{}, err
	}
	if captured != 1 || childSession.Completeness != "complete" && childSession.Completeness != "indexed-source" {
		out.State = "child-evidence-incomplete"
		out.ChildSessionID = ""
		out.ChildSnapshot = ""
		return out, nil
	}
	out.State = "resolved"
	out.ChildAgentID = "main"
	if parent.Provider == "claude" {
		out.ChildAgentID = child
	}
	return out, nil
}

// Claude's structured toolUseResult belongs to one tool_result block. Reject
// multi-result envelopes rather than assigning an agent ID to the wrong call.
func claudeSpawnChild(raw []byte, callID, sessionID, caller string) (string, string) {
	record, ok := delegationJSONObject(raw)
	str := func(raw json.RawMessage) string { var value string; _ = json.Unmarshal(raw, &value); return value }
	if !ok || str(record["type"]) != "user" || sessionID == "" || str(record["sessionId"]) != sessionID || callID == "" {
		return "", "source-record-mismatch"
	}
	agent := str(record["agentId"])
	if agent == "" {
		agent = "main"
	}
	if agent != caller {
		return "", "source-record-mismatch"
	}
	message, ok := delegationJSONObject(record["message"])
	if !ok {
		return "", "unsupported-result"
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || len(blocks) > 4096 {
		return "", "unsupported-result"
	}
	results := 0
	for _, block := range blocks {
		value, valid := delegationJSONObject(block)
		if !valid {
			return "", "unsupported-result"
		}
		if str(value["type"]) == "tool_result" {
			results++
			if str(value["tool_use_id"]) != callID {
				return "", "source-record-mismatch"
			}
		}
	}
	if results != 1 {
		return "", "source-record-mismatch"
	}
	result, ok := delegationJSONObject(record["toolUseResult"])
	if !ok {
		return "", "unsupported-result"
	}
	child := str(result["agentId"])
	if !validID(child) || strings.ContainsRune(child, '\uFFFD') {
		return "", "unsupported-result"
	}
	return child, "known"
}

// Only the explicit structured agent_id result is supported here. Never infer
// an ID from prose, a display preview, a file path or an agent name.
func codexSpawnChild(raw []byte, callID string) (string, string) {
	str := func(raw json.RawMessage) string { var s string; _ = json.Unmarshal(raw, &s); return s }
	record, ok := delegationJSONObject(raw)
	if !ok || str(record["type"]) != "response_item" || callID == "" {
		return "", "source-record-mismatch"
	}
	payload, ok := delegationJSONObject(record["payload"])
	if !ok || str(payload["type"]) != "function_call_output" || str(payload["call_id"]) != callID {
		return "", "source-record-mismatch"
	}
	output := payload["output"]
	var text string
	if json.Unmarshal(output, &text) == nil {
		output = []byte(text)
	}
	value, ok := delegationJSONObject(output)
	if !ok {
		return "", "unsupported-result"
	}
	var child string
	if json.Unmarshal(value["agent_id"], &child) != nil || !validID(child) || strings.ContainsRune(child, '\uFFFD') {
		return "", "unsupported-result"
	}
	return child, "known"
}

func delegationJSONObject(raw []byte) (map[string]json.RawMessage, bool) {
	if len(raw) > 8<<20 || !utf8.Valid(raw) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	result := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || len(result) >= 4096 {
			return nil, false
		}
		if _, exists := result[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		result[key] = value
	}
	if _, err = d.Token(); err != nil || d.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return result, true
}
