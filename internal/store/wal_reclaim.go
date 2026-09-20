package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// Try only after a successful passive checkpoint of a large WAL. SQLite owns
// all reset/truncation locks: an active reader/writer makes this back off, not
// wait for that transaction. Filesystem flushes remain cooperatively bounded.
func (s *Store) reclaimWAL(ctx context.Context) (reclaimed bool, err error) {
	return s.recycleWAL(ctx, false)
}

// restartWAL attempts to make the next writer reset a large WAL. Its five-ms
// busy handler limits lock waiting only, not underlying filesystem operations.
// Existing readers retain their snapshots; this never forces truncation.
func (s *Store) restartWAL(ctx context.Context) (bool, error) {
	return s.recycleWAL(ctx, true)
}

func (s *Store) recycleWAL(ctx context.Context, restart bool) (reclaimed bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var timeout int
	if err = conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		return false, err
	}
	defer func() {
		// Restore even when the attempt's context expired. If restoration fails,
		// discard this physical connection instead of leaking its changed policy.
		restore, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, resetErr := conn.ExecContext(restore, fmt.Sprintf("PRAGMA busy_timeout=%d", timeout)); resetErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, resetErr)
		}
	}()
	policy, command := "PRAGMA busy_timeout=0", "PRAGMA wal_checkpoint(TRUNCATE)"
	if restart {
		policy, command = "PRAGMA busy_timeout=5", "PRAGMA wal_checkpoint(RESTART)"
	}
	if _, err = conn.ExecContext(ctx, policy); err != nil {
		return false, err
	}
	var busy, frames, copied int
	if err = conn.QueryRowContext(ctx, command).Scan(&busy, &frames, &copied); err != nil {
		return false, err
	}
	if restart {
		return busy == 0 && frames == copied, nil
	}
	return busy == 0 && frames == 0 && copied == 0, nil
}
