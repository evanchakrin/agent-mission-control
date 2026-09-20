package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func supportedSQLite(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	numbers := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		numbers[i] = n
	}
	for i, n := range [3]int{3, 51, 3} {
		if numbers[i] > n {
			return true
		}
		if numbers[i] < n {
			return false
		}
	}
	return true
}

func (s *Store) SetExternalIndex(ctx context.Context, sourceID string, external bool) error {
	if !validID(sourceID) {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO properties(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "external_index:"+sourceID, strconv.FormatBool(external))
	return err
}

type LegacyRequest struct {
	SourceID   string
	Generation string
	Hash       string
	Offset     int64
	Length     int64
	Complete   bool
}

// ReserveLegacyRequest frames otherwise unframed OTLP JSON bytes. The request
// hash deduplicates retries, and the durable frame allows crash recovery before
// direct normalization without reparsing an arbitrary concatenated JSON stream.
func (s *Store) ReserveLegacyRequest(ctx context.Context, r LegacyRequest) (LegacyRequest, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS legacy_requests(source_id TEXT NOT NULL,generation TEXT NOT NULL,hash TEXT NOT NULL,offset INTEGER NOT NULL,length INTEGER NOT NULL,complete INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(source_id,generation,hash),UNIQUE(source_id,generation,offset))`); err != nil {
		return r, err
	}
	var old LegacyRequest
	old.SourceID = r.SourceID
	old.Generation = r.Generation
	old.Hash = r.Hash
	err = tx.QueryRowContext(ctx, `SELECT offset,length,complete FROM legacy_requests WHERE source_id=? AND generation=? AND hash=?`, r.SourceID, r.Generation, r.Hash).Scan(&old.Offset, &old.Length, &old.Complete)
	if err == nil {
		if old.Length != r.Length {
			return old, ErrConflict
		}
		return old, nil
	}
	if err != sql.ErrNoRows {
		return r, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO legacy_requests(source_id,generation,hash,offset,length) VALUES(?,?,?,?,?)`, r.SourceID, r.Generation, r.Hash, r.Offset, r.Length)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}
func (s *Store) PendingLegacyRequests(ctx context.Context, sourceID, generation string) ([]LegacyRequest, error) {
	// Creation here also permits a collector restart before any first request.
	s.writeMu.Lock()
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS legacy_requests(source_id TEXT NOT NULL,generation TEXT NOT NULL,hash TEXT NOT NULL,offset INTEGER NOT NULL,length INTEGER NOT NULL,complete INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(source_id,generation,hash),UNIQUE(source_id,generation,offset))`)
	s.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT hash,offset,length FROM legacy_requests WHERE source_id=? AND generation=? AND complete=0 ORDER BY offset`, sourceID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LegacyRequest{}
	for rows.Next() {
		r := LegacyRequest{SourceID: sourceID, Generation: generation}
		if err = rows.Scan(&r.Hash, &r.Offset, &r.Length); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) CompleteLegacyRequest(ctx context.Context, r LegacyRequest) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE legacy_requests SET complete=1 WHERE source_id=? AND generation=? AND hash=?`, r.SourceID, r.Generation, r.Hash)
	return err
}

func (s *Store) MarkLegacyDelta(ctx context.Context, sourceID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO properties(key,value) VALUES(?,'true')`, "legacy_delta:"+sourceID)
	return err
}
func (s *Store) LegacyDeltaOwned(ctx context.Context, sourceID string) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key=?`, "legacy_delta:"+sourceID).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return value == "true", err
}

func (s *Store) LegacySnapshot(ctx context.Context, sourceID string) (generation, hash string, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key=?`, "legacy_snapshot:"+sourceID).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	var parts []string
	if json.Unmarshal([]byte(raw), &parts) != nil || len(parts) != 2 {
		return "", "", fmt.Errorf("corrupt legacy snapshot receipt")
	}
	return parts[0], parts[1], nil
}
func (s *Store) RememberLegacySnapshot(ctx context.Context, sourceID, generation, hash string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b, _ := json.Marshal([]string{generation, hash})
	_, err := s.db.ExecContext(ctx, `INSERT INTO properties(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "legacy_snapshot:"+sourceID, string(b))
	return err
}

func (s *Store) TouchLegacyMachine(ctx context.Context, machine string) error {
	if !validID(machine) {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := stamp(time.Now())
	b, _ := json.Marshal(protocol.Heartbeat{MachineID: machine, Name: machine, State: "legacy-connected"})
	_, err := s.db.ExecContext(ctx, `INSERT INTO machines VALUES(?,?,?) ON CONFLICT(machine_id) DO UPDATE SET last_seen=excluded.last_seen`, machine, b, now)
	return err
}

func (s *Store) RememberLegacyEnvelope(ctx context.Context, sourceID, generation string, offset int64, payload []byte) error {
	if len(payload) > 16384 || !json.Valid(payload) {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS legacy_envelopes(source_id TEXT NOT NULL,generation TEXT NOT NULL,offset INTEGER NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(source_id,generation,offset,payload))`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_envelopes VALUES(?,?,?,?)`, sourceID, generation, offset, payload); err != nil {
		return err
	}
	return tx.Commit()
}

// CurrentSource returns the active raw generation without relying on filename
// order or process-local state. Older generations remain available by ID.
func (s *Store) CurrentSource(ctx context.Context, sourceID string) (SourceState, error) {
	var generation string
	err := s.db.QueryRowContext(ctx, `SELECT active_generation FROM source_identity WHERE source_id=?`, sourceID).Scan(&generation)
	if err == sql.ErrNoRows {
		return SourceState{}, ErrNotFound
	}
	if err != nil {
		return SourceState{}, err
	}
	return s.SourceState(ctx, sourceID, generation)
}

func (s *Store) LegacySource(ctx context.Context, sourceID string) (protocol.Source, error) {
	var source protocol.Source
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key=?`, "legacy_source:"+sourceID).Scan(&b)
	if err == sql.ErrNoRows {
		var target string
		err = s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key=?`, "legacy_route:"+sourceID).Scan(&target)
		if err == sql.ErrNoRows {
			return source, ErrNotFound
		}
		if err != nil {
			return source, err
		}
		return s.LegacySource(ctx, target)
	}
	if err != nil {
		return source, err
	}
	err = json.Unmarshal(b, &source)
	return source, err
}

func (s *Store) RememberLegacyRoute(ctx context.Context, routeID string, source protocol.Source) error {
	if err := s.RememberLegacySource(ctx, source); err != nil {
		return err
	}
	if routeID == source.SourceID {
		return nil
	}
	if !validID(routeID) {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO properties(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "legacy_route:"+routeID, source.SourceID)
	return err
}

// RememberLegacySource persists the v1 adapter's generation reservation before
// the first chunk, so failed replacement uploads resume the same generation.
func (s *Store) RememberLegacySource(ctx context.Context, source protocol.Source) error {
	if !validID(source.SourceID) || !validID(source.MachineID) || !validID(source.Generation) || source.GenerationSequence < 0 {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM properties WHERE key=?`, "legacy_source:"+source.SourceID).Scan(&b)
	if err == nil {
		var previous protocol.Source
		if err = json.Unmarshal(b, &previous); err != nil {
			return err
		}
		if previous.MachineID != source.MachineID || previous.Provider != source.Provider || previous.NativeID != source.NativeID || previous.GenerationSequence > source.GenerationSequence {
			return fmt.Errorf("%w: legacy source reservation", ErrConflict)
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	b, err = json.Marshal(source)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO properties(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "legacy_source:"+source.SourceID, string(b)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutLegacyAlias(ctx context.Context, key, sessionID string) error {
	if !validID(key) || !validID(sessionID) {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT session_id FROM legacy_aliases WHERE legacy_key=?`, key).Scan(&old)
	if err == nil {
		if old != sessionID {
			if old == key {
				return adoptLegacyPlaceholder(ctx, tx, key, sessionID)
			}
			return fmt.Errorf("%w: alias already identifies another session", ErrConflict)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO legacy_aliases VALUES(?,?)`, key, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// A migration may retain organization before its transcript arrives. Adopt only
// that untouched placeholder, never a real session or an owner-edited identity.
// Keep the original rows as evidence; destination owner state always wins.
func adoptLegacyPlaceholder(ctx context.Context, tx *sql.Tx, key, id string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='legacy_metadata'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: alias is not an imported placeholder", ErrConflict)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM legacy_metadata WHERE legacy_key=? AND EXISTS(SELECT 1 FROM session_metadata WHERE session_id=? AND revision=1) AND NOT EXISTS(SELECT 1 FROM sessions WHERE id=?) AND NOT EXISTS(SELECT 1 FROM metadata_operations WHERE session_id=?)`, key, key, key, key).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return fmt.Errorf("%w: legacy placeholder requires owner reconciliation", ErrConflict)
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_metadata(session_id,revision,value) SELECT ?,revision,value FROM session_metadata WHERE session_id=?`, id, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_names(session_id,agent_id,name) SELECT ?,agent_id,name FROM agent_names WHERE session_id=?`, id, key); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('metadata',?,?)`, id, stamp(time.Now().UTC())); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE legacy_aliases SET session_id=? WHERE legacy_key=? AND session_id=?`, id, key, key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LegacyManifest(ctx context.Context, machine string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.value,s.durable_offset FROM properties p LEFT JOIN sources s ON s.source_id=json_extract(p.value,'$.sourceId') AND s.generation=json_extract(p.value,'$.generation') WHERE p.key LIKE 'legacy_source:%' AND json_extract(p.value,'$.machineId')=?`, machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var b []byte
		var offset sql.NullInt64
		if err = rows.Scan(&b, &offset); err != nil {
			return nil, err
		}
		var src protocol.Source
		if err = json.Unmarshal(b, &src); err != nil {
			return nil, err
		}
		if offset.Valid {
			out[src.Path] = offset.Int64
		}
	}
	return out, rows.Err()
}
