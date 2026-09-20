package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

// Explicitly selected, query-only audit of an offline restored installation.
// Reports counts and digests, never chats, metadata values or credentials.
func TestReadOnlyRestoredInstallationAudit(t *testing.T) {
	source, dest := os.Getenv("AMC_AUDIT_BACKUP"), os.Getenv("AMC_AUDIT_RESTORED")
	if source == "" || dest == "" {
		t.Skip("backup and offline restored installation not selected")
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(dest) || source == dest {
		t.Fatal("distinct absolute directories required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	open := func(path string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		return db
	}
	before := open(filepath.Join(source, "hub", "ledger.sqlite"))
	after := open(filepath.Join(dest, "hub", "ledger.sqlite"))
	for _, key := range []string{"hub_id", "recovery_epoch"} {
		var a, b string
		if err := before.QueryRowContext(ctx, "SELECT value FROM properties WHERE key=?", key).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := after.QueryRowContext(ctx, "SELECT value FROM properties WHERE key=?", key).Scan(&b); err != nil {
			t.Fatal(err)
		}
		if (key == "hub_id" && a != b) || (key == "recovery_epoch" && (a == b || b == "")) {
			t.Fatal("restore identity contract failed", key)
		}
	}
	digest := func(db *sql.DB, query string) (int64, string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		encoder := json.NewEncoder(hash)
		if err = encoder.Encode(columns); err != nil {
			t.Fatal(err)
		}
		var count int64
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			if err = encoder.Encode(values); err != nil {
				t.Fatal(err)
			}
			count++
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return count, hex.EncodeToString(hash.Sum(nil))
	}
	for _, entry := range []struct{ name, query string }{
		{"sources", "SELECT * FROM sources ORDER BY source_id,generation"},
		{"source_identity", "SELECT * FROM source_identity ORDER BY source_id"},
		{"chunks", "SELECT * FROM chunks ORDER BY source_id,generation,offset"},
		{"sessions", "SELECT * FROM sessions ORDER BY id"},
		{"events", "SELECT * FROM events ORDER BY seq"},
		{"usage_observations", "SELECT * FROM usage_observations ORDER BY id"},
		{"session_metadata", "SELECT * FROM session_metadata ORDER BY session_id"},
		{"metadata_operations", "SELECT * FROM metadata_operations ORDER BY operation_id"},
		{"organization_audit", "SELECT * FROM organization_audit ORDER BY operation_id"},
		{"legacy_aliases", "SELECT * FROM legacy_aliases ORDER BY legacy_key"},
		{"projection_revisions", "SELECT * FROM projection_revisions ORDER BY revision"},
	} {
		a, ha := digest(before, entry.query)
		b, hb := digest(after, entry.query)
		if a != b || ha != hb {
			t.Fatal("logical restore mismatch", entry.name)
		}
		t.Logf("table=%s rows=%d digest=%s matched", entry.name, a, ha)
	}
	// Owner restore copies the database verbatim and must not replay file writes.
	a, err := fileHash(filepath.Join(source, "desktop", "owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fileHash(filepath.Join(dest, "desktop", "owner.db"))
	if err != nil || a != b {
		t.Fatal("owner database changed", err)
	}
	for _, name := range []string{"hub-config.json", "desktop-config.json"} {
		want, err := platform.LoadConfig(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := platform.LoadConfig(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		want.DataDir = filepath.Join(dest, string(want.Role))
		if !reflect.DeepEqual(want, got) {
			t.Fatal("restored configuration differs beyond data directory", name)
		}
	}
	rows, err := after.QueryContext(ctx, "SELECT sha256,MIN(length),MAX(length) FROM chunks GROUP BY sha256 ORDER BY sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var blobs, rawBytes int64
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			t.Fatal(err)
		}
		var expected string
		var low, high int64
		if err = rows.Scan(&expected, &low, &high); err != nil {
			t.Fatal(err)
		}
		decoded, e := hex.DecodeString(expected)
		if e != nil || len(decoded) != 32 || low != high {
			t.Fatal("invalid chunk digest/length")
		}
		path := filepath.Join(dest, "hub", "blobs", expected[:2], expected)
		stat, e := os.Stat(path)
		if e != nil || stat.Size() != high {
			t.Fatal("restored blob size mismatch", e)
		}
		actual, e := fileHash(path)
		if e != nil || actual != expected {
			t.Fatal("restored blob checksum mismatch", e)
		}
		blobs++
		rawBytes += high
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("verified restored raw blobs=%d bytes=%d; stable hub identity, rotated recovery epoch, owner database and rewritten configurations matched", blobs, rawBytes))
}
