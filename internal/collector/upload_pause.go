package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// No server-supplied text or credentials are persisted in this diagnostic.
type uploadPause struct {
	Until    int64  `json:"until"`
	State    string `json:"state"`
	Failures int    `json:"failures,omitempty"`
}

type pauseReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readUploadPause(ctx context.Context, db pauseReader) (uploadPause, error) {
	var p uploadPause
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='upload_pause'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}

func writeUploadPause(ctx context.Context, tx *sql.Tx, p uploadPause) error {
	previous, err := readUploadPause(ctx, tx)
	if err != nil {
		return err
	}
	if previous.Until > p.Until {
		return nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('upload_pause',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(raw))
	return err
}
