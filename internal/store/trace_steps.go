package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

type TraceStep struct {
	Sequence  int64  `json:"sequence"`
	Tool      string `json:"tool"`
	Preview   string `json:"preview"`
	State     string `json:"state"`
	Signature string `json:"signature,omitempty"`
}
type TracePage struct {
	Steps        []TraceStep `json:"steps"`
	Snapshot     string      `json:"snapshot"`
	NextSequence int64       `json:"nextSequence,omitempty"`
	Completeness string      `json:"completeness"`
	Comparison   string      `json:"comparison"`
}

// TraceSteps compares complete captured arguments, not abbreviated event text.
// Pages bound both references and raw I/O; callers can continue beyond any page.
func (s *Store) TraceSteps(ctx context.Context, id, agent string, after int64, limit int, expected string) (TracePage, error) {
	out := TracePage{Steps: []TraceStep{}, Comparison: "exact-tool-and-json-arguments-v1"}
	if id == "" || len(id) > 4096 || agent == "" || len(agent) > 4096 || after < 0 || limit < 1 || limit > 100 || len(expected) > 128 || after > 0 && expected == "" {
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
	out.Completeness = parent.Completeness
	page, err := s.queryEvents(ctx, toolCallWhere+" AND e.agent_id=?", []any{id, after, agent}, limit, false)
	if err != nil {
		return out, err
	}
	var consumed int64
	for _, e := range page.Events {
		if err = ctx.Err(); err != nil {
			return TracePage{}, err
		}
		if e.SourceLength <= 8<<20 && consumed+e.SourceLength > 16<<20 && len(out.Steps) > 0 {
			out.NextSequence = out.Steps[len(out.Steps)-1].Sequence
			break
		}
		var data struct {
			Tool string `json:"tool"`
			ID   string `json:"toolUseId"`
		}
		_ = json.Unmarshal(e.Data, &data)
		step := TraceStep{Sequence: e.Sequence, Tool: data.Tool, Preview: e.Text, State: "missing-source"}
		switch {
		case parent.Completeness != "complete" && parent.Completeness != "indexed-source":
			step.State = "incomplete-evidence"
		case e.SourceLength <= 0:
		case e.SourceLength > 8<<20:
			step.State = "oversize-source"
		default:
			raw, openErr := s.OpenSource(ctx, parent.SourceID, parent.Generation, e.SourceOffset)
			if openErr != nil {
				return TracePage{}, openErr
			}
			body := make([]byte, int(e.SourceLength))
			_, err = io.ReadFull(raw, body)
			closeErr := raw.Close()
			if err != nil {
				return TracePage{}, err
			}
			if closeErr != nil {
				return TracePage{}, closeErr
			}
			consumed += e.SourceLength
			step.State, step.Signature = traceSignature(parent.Provider, body, data.Tool, data.ID)
		}
		out.Steps = append(out.Steps, step)
	}
	if out.NextSequence == 0 {
		out.NextSequence = page.NextSequence
	}
	if _, err = s.historySnapshot(ctx, id, out.Snapshot); err != nil {
		return TracePage{}, err
	}
	return out, nil
}

func traceSignature(provider string, raw []byte, tool, callID string) (string, string) {
	if tool == "" || callID == "" {
		return "missing-call-identity", ""
	}
	record, ok := delegationJSONObject(raw)
	if !ok {
		return "unsupported-source", ""
	}
	str := func(raw json.RawMessage) string { var v string; _ = json.Unmarshal(raw, &v); return v }
	var arguments []byte
	switch provider {
	case "codex":
		payload, valid := delegationJSONObject(record["payload"])
		if !valid || str(record["type"]) != "response_item" || str(payload["type"]) != "function_call" || str(payload["call_id"]) != callID || str(payload["name"]) != tool {
			return "source-record-mismatch", ""
		}
		var value string
		if json.Unmarshal(payload["arguments"], &value) != nil || strings.ContainsRune(value, '\uFFFD') {
			return "unsupported-arguments", ""
		}
		arguments = []byte(value)
	case "claude":
		message, valid := delegationJSONObject(record["message"])
		if !valid || str(record["type"]) != "assistant" {
			return "unsupported-source", ""
		}
		var blocks []json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil || len(blocks) > 4096 {
			return "unsupported-source", ""
		}
		matches := 0
		for _, block := range blocks {
			value, valid := delegationJSONObject(block)
			if !valid {
				return "unsupported-source", ""
			}
			if str(value["type"]) == "tool_use" && str(value["id"]) == callID {
				if str(value["name"]) != tool {
					return "source-record-mismatch", ""
				}
				matches++
				arguments = value["input"]
			}
		}
		if matches != 1 {
			return "source-record-mismatch", ""
		}
	default:
		return "unsupported-provider", ""
	}
	if len(arguments) == 0 || !json.Valid(arguments) {
		return "unsupported-arguments", ""
	}
	var compact bytes.Buffer
	if json.Compact(&compact, arguments) != nil {
		return "unsupported-arguments", ""
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("amc-trace-step-v1\x00" + tool + "\x00"))
	_, _ = hash.Write(compact.Bytes())
	return "known", hex.EncodeToString(hash.Sum(nil))
}
