package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// MachineLabel is owner organization state. It never replaces the collector's
// reported name or the stable identity used by source and session records.
type MachineLabel struct {
	MachineID   string `json:"machineId"`
	DisplayName string `json:"displayName"`
	Revision    int64  `json:"revision"`
	UpdatedAt   string `json:"updatedAt"`
}

// Legacy remote machine names are the stable IDs retained by the shadow
// importer. Never interpret a friendly label as an ID or replace owner state.
func (s *Store) ImportLegacyMachineLabels(ctx context.Context, names map[string]string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, name := range names {
		if !validID(id) || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return ErrInvalid
		}
		now := stamp(time.Now().UTC())
		label := MachineLabel{MachineID: id, DisplayName: strings.TrimSpace(name), Revision: 1, UpdatedAt: now}
		result, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO machine_labels VALUES(?,?,?,?)`, id, label.DisplayName, label.Revision, now)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		digest := sha256.Sum256([]byte(id))
		op := "legacy-machine-label:" + hex.EncodeToString(digest[:])
		before, _ := json.Marshal(MachineLabel{MachineID: id})
		after, _ := json.Marshal(label)
		if _, err = tx.ExecContext(ctx, `INSERT INTO machine_label_audit VALUES(?,?,?,?,?,?)`, id, 1, op, before, after, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('machine-label','',?)`, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type MachineLabelMutation struct {
	MachineID     string `json:"machineId"`
	DisplayName   string `json:"displayName"`
	Revision      int64  `json:"revision"`
	OperationID   string `json:"operationId"`
	RecoveryEpoch string `json:"recoveryEpoch,omitempty"`
}

func (s *Store) MachineLabel(ctx context.Context, id string) (MachineLabel, error) {
	result := MachineLabel{MachineID: id}
	if !validID(id) {
		return result, ErrInvalid
	}
	err := s.db.QueryRowContext(ctx, `SELECT display_name,revision,updated_at FROM machine_labels WHERE machine_id=?`, id).Scan(&result.DisplayName, &result.Revision, &result.UpdatedAt)
	if err == sql.ErrNoRows {
		err = nil
	}
	return result, err
}

// Blank labels retain a revisioned tombstone so a stale rename cannot resurrect
// a cleared override. Receipt, audit, label and change notification commit once.
func (s *Store) MutateMachineLabel(ctx context.Context, p MachineLabelMutation) (MachineLabel, error) {
	var empty MachineLabel
	if !validID(p.MachineID) || !validID(p.OperationID) || p.Revision < 0 || len(p.DisplayName) > 1024 || !utf8.ValidString(p.DisplayName) || strings.ContainsRune(p.DisplayName, 0) || len(p.RecoveryEpoch) > 128 {
		return empty, ErrInvalid
	}
	request, err := json.Marshal(p)
	if err != nil {
		return empty, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var prior, result []byte
	err = tx.QueryRowContext(ctx, `SELECT request,result FROM machine_label_operations WHERE operation_id=?`, p.OperationID).Scan(&prior, &result)
	if err == nil {
		if !bytes.Equal(prior, request) {
			return empty, ErrConflict
		}
		err = json.Unmarshal(result, &empty)
		return empty, err
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	if p.RecoveryEpoch != "" && p.RecoveryEpoch != s.epoch {
		return empty, ErrHistoryChanged
	}
	old := MachineLabel{MachineID: p.MachineID}
	err = tx.QueryRowContext(ctx, `SELECT display_name,revision,updated_at FROM machine_labels WHERE machine_id=?`, p.MachineID).Scan(&old.DisplayName, &old.Revision, &old.UpdatedAt)
	if err != nil && err != sql.ErrNoRows {
		return empty, err
	}
	if old.Revision != p.Revision {
		return old, ErrConflict
	}
	now := stamp(time.Now().UTC())
	updated := MachineLabel{MachineID: p.MachineID, DisplayName: strings.TrimSpace(p.DisplayName), Revision: old.Revision + 1, UpdatedAt: now}
	before, _ := json.Marshal(old)
	after, _ := json.Marshal(updated)
	if _, err = tx.ExecContext(ctx, `INSERT INTO machine_labels VALUES(?,?,?,?) ON CONFLICT(machine_id) DO UPDATE SET display_name=excluded.display_name,revision=excluded.revision,updated_at=excluded.updated_at`, p.MachineID, updated.DisplayName, updated.Revision, now); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO machine_label_audit VALUES(?,?,?,?,?,?)`, p.MachineID, updated.Revision, p.OperationID, before, after, now); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO machine_label_operations VALUES(?,?,?)`, p.OperationID, request, after); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('machine-label','',?)`, now); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return updated, nil
}
