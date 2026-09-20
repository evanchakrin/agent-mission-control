package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

const delegationTaskSchema = `CREATE TABLE IF NOT EXISTS delegation_tasks_v1 (
 event_sequence INTEGER PRIMARY KEY REFERENCES events(seq) ON DELETE CASCADE,
 state TEXT NOT NULL, prompt_hash TEXT NOT NULL, normalized_bytes INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS delegation_tasks_v1_match ON delegation_tasks_v1(prompt_hash,event_sequence) WHERE state='known';
CREATE TRIGGER IF NOT EXISTS delegation_tasks_v1_changed AFTER UPDATE ON events BEGIN DELETE FROM delegation_tasks_v1 WHERE event_sequence=OLD.seq; END;
INSERT OR IGNORE INTO properties(key,value) VALUES('delegation_revision','0');
CREATE TRIGGER IF NOT EXISTS delegation_tasks_v1_insert_revision AFTER INSERT ON delegation_tasks_v1 BEGIN UPDATE properties SET value=CAST(value AS INTEGER)+1 WHERE key='delegation_revision'; END;
CREATE TRIGGER IF NOT EXISTS delegation_tasks_v1_update_revision AFTER UPDATE ON delegation_tasks_v1 BEGIN UPDATE properties SET value=CAST(value AS INTEGER)+1 WHERE key='delegation_revision'; END;
CREATE TRIGGER IF NOT EXISTS delegation_tasks_v1_delete_revision AFTER DELETE ON delegation_tasks_v1 BEGIN UPDATE properties SET value=CAST(value AS INTEGER)+1 WHERE key='delegation_revision'; END;`

// This indexes complete parser evidence, never the display preview. Raw bytes
// remain authoritative. Missing legacy entries require reindexing, not guessing.
func saveDelegationTask(ctx context.Context, tx *sql.Tx, e Event, seq int64) error {
	if e.Kind != "tool-call" {
		return nil
	}
	var data struct {
		Tool string `json:"tool"`
	}
	if json.Unmarshal(e.Data, &data) != nil {
		return nil
	}
	field := ""
	switch data.Tool {
	case "Task", "Agent":
		field = "prompt"
	case "spawn_agent", "functions.spawn_agent", "collaboration.spawn_agent":
		field = "message"
	default:
		return nil
	}
	state, fingerprint, size := delegationFingerprint(e.SearchText, field)
	if e.SourceLength <= 0 {
		state, fingerprint, size = "missing-source", "", 0
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO delegation_tasks_v1 VALUES(?,?,?,?)`, seq, state, fingerprint, size)
	return err
}

// Match complete instructions after whitespace normalization, not descriptions,
// prefixes, summaries or embeddings. Bound input to the parser's record ceiling.
func delegationFingerprint(arguments, field string) (string, string, int) {
	if arguments == "" {
		return "missing-full-text", "", 0
	}
	if len(arguments) > 8<<20 {
		return "oversize", "", 0
	}
	if !utf8.ValidString(arguments) {
		return "invalid-arguments", "", 0
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "invalid-arguments", "", 0
	}
	seen := map[string]bool{}
	var prompt string
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return "invalid-arguments", "", 0
		}
		seen[key] = true
		if len(seen) > 4096 {
			return "field-limit", "", 0
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return "invalid-arguments", "", 0
		}
		if key == field && json.Unmarshal(raw, &prompt) != nil {
			return "invalid-arguments", "", 0
		}
	}
	if _, err = decoder.Token(); err != nil {
		return "invalid-arguments", "", 0
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "invalid-arguments", "", 0
	}
	if !seen[field] {
		return "missing-instruction", "", 0
	}
	// encoding/json replaces malformed surrogate escapes with U+FFFD. Refuse
	// ambiguous decoded evidence rather than letting distinct raw inputs match.
	if strings.ContainsRune(prompt, '\uFFFD') {
		return "ambiguous-unicode", "", 0
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("amc-delegation-prompt-v1\x00"))
	size, runes := 0, 0
	for word := range strings.FieldsSeq(prompt) {
		if size > 0 {
			_, _ = hash.Write([]byte{' '})
			size++
			runes++
		}
		_, _ = hash.Write([]byte(word))
		size += len(word)
		runes += utf8.RuneCountInString(word)
	}
	if runes < 60 {
		return "too-vague", "", size
	}
	return "known", hex.EncodeToString(hash.Sum(nil)), size
}
