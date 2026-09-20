package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDelegationFingerprintUsesWholeInstructions(t *testing.T) {
	encode := func(prompt string) string {
		b, _ := json.Marshal(map[string]string{"prompt": prompt})
		return string(b)
	}
	prompt := strings.Repeat("same visible beginning ", 100) + "first ending"
	state, first, n := delegationFingerprint(encode(prompt), "prompt")
	if state != "known" || len(first) != 64 || n != len(prompt) {
		t.Fatal(state, first, n)
	}
	_, second, _ := delegationFingerprint(encode(strings.Repeat("same visible beginning ", 100)+"different ending"), "prompt")
	if first == second {
		t.Fatal("instructions matched only their preview")
	}
	_, spaced, _ := delegationFingerprint(encode("  "+strings.ReplaceAll(prompt, " ", "\n\t")+"  "), "prompt")
	if spaced != first {
		t.Fatal("whitespace normalization changed")
	}
	for _, fixture := range []struct{ raw, state string }{
		{"", "missing-full-text"}, {`{"description":"do the same work"}`, "missing-instruction"},
		{`{"prompt":"short"}`, "too-vague"}, {`{"prompt":null}`, "too-vague"},
		{`{"prompt":7}`, "invalid-arguments"}, {`{"prompt":"one","prompt":"two"}`, "invalid-arguments"},
		{`[]`, "invalid-arguments"}, {`{} {}`, "invalid-arguments"}, {`{"prompt":"\ud800"}`, "ambiguous-unicode"},
		{strings.Repeat("x", (8<<20)+1), "oversize"},
	} {
		state, hash, _ := delegationFingerprint(fixture.raw, "prompt")
		if state != fixture.state || hash != "" {
			t.Fatal(state, fixture.state, hash)
		}
	}
}

func TestDelegationTasksRefusePreviewAndInvalidateMutations(t *testing.T) {
	for _, fixture := range []struct{ mode, state string }{{"preview", "missing-full-text"}, {"source", "missing-source"}, {"known", "known"}} {
		t.Run(fixture.mode, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			full, _ := json.Marshal(map[string]string{"message": strings.Repeat("source instruction ", 6)})
			e := Event{ID: "spawn", Kind: "tool-call", SourceLength: 2, Text: string(full), SearchText: string(full), Data: json.RawMessage(`{"tool":"collaboration.spawn_agent"}`)}
			if fixture.mode == "preview" {
				e.SearchText = ""
			}
			if fixture.mode == "source" {
				e.SourceLength = 0
			}
			b.Events = []Event{e}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			var state, hash string
			if err := s.db.QueryRowContext(ctx, `SELECT state,prompt_hash FROM delegation_tasks_v1`).Scan(&state, &hash); err != nil || state != fixture.state {
				t.Fatal(state, err)
			}
			if (hash != "") != (fixture.mode == "known") {
				t.Fatal("unproven matching fingerprint", hash)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE events SET text='changed interpretation'`); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delegation_tasks_v1`).Scan(&n); err != nil || n != 0 {
				t.Fatal("mutation retained stale task evidence", n, err)
			}
		})
	}
}

func TestDelegationTasksCommitDeduplicationAndRollback(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\ny\n")
	b := batch(src, 0, 2)
	full, _ := json.Marshal(map[string]string{"prompt": strings.Repeat("complete instruction ", 6)})
	b.Events = []Event{{ID: "task", Kind: "tool-call", SourceLength: 2, Text: "truncated preview", SearchText: string(full), Data: json.RawMessage(`{"tool":"Task"}`)}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	var state, hash string
	if err := s.db.QueryRowContext(ctx, `SELECT state,prompt_hash FROM delegation_tasks_v1`).Scan(&state, &hash); err != nil || state != "known" || len(hash) != 64 {
		t.Fatal(state, hash, err)
	}
	next := batch(src, 2, 4)
	next.Events = b.Events
	if err := s.CommitIndex(ctx, next); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delegation_tasks_v1`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delegation_tasks_v1`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rollback lost task evidence", count, err)
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delegation_tasks_v1`).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan task evidence", count, err)
	}
}
