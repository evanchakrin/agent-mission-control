package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

var errReclaimRestoreFixture = errors.New("injected timeout restoration failure")

type reclaimFailureConnector struct {
	driver.Connector
	failRestore  atomic.Bool
	closed       atomic.Int32
	restores     atomic.Int32
	cancelOnZero context.CancelFunc
}

func (c *reclaimFailureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &reclaimFailureConn{Conn: conn, owner: c}, nil
}

type reclaimFailureConn struct {
	driver.Conn
	owner *reclaimFailureConnector
}

func (c *reclaimFailureConn) Close() error { c.owner.closed.Add(1); return c.Conn.Close() }
func (c *reclaimFailureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "PRAGMA busy_timeout=5000" {
		c.owner.restores.Add(1)
		if c.owner.failRestore.CompareAndSwap(true, false) {
			return nil, errReclaimRestoreFixture
		}
	}
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err == nil && (query == "PRAGMA busy_timeout=0" || query == "PRAGMA busy_timeout=5") && c.owner.cancelOnZero != nil {
		c.owner.cancelOnZero()
	}
	return result, err
}

func TestWALRestartDiscardsConnectionWhenRestorationFails(t *testing.T) {
	s, connector := reclaimFailureStore(t)
	connector.failRestore.Store(true)
	if _, err := s.restartWAL(context.Background()); !errors.Is(err, errReclaimRestoreFixture) {
		t.Fatal(err)
	}
	if connector.closed.Load() != 1 || s.ConnectionStats().OpenConnections != 0 {
		t.Fatal("altered restart connection returned to pool", connector.closed.Load(), s.ConnectionStats())
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal(timeout, err)
	}
}

func TestWALRestartRestoresPolicyAfterMidAttemptCancellation(t *testing.T) {
	s, connector := reclaimFailureStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector.cancelOnZero = cancel
	if ok, err := s.restartWAL(ctx); ok || !errors.Is(err, context.Canceled) {
		t.Fatal(ok, err)
	}
	if connector.restores.Load() != 1 {
		t.Fatal("cancellation skipped restoration", connector.restores.Load())
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal(timeout, err)
	}
}

func reclaimFailureStore(t *testing.T) (*Store, *reclaimFailureConnector) {
	t.Helper()
	dir := t.TempDir()
	base, err := sqlite.NewConnector(filepath.ToSlash(filepath.Join(dir, "ledger.sqlite")) + "?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		t.Fatal(err)
	}
	connector := &reclaimFailureConnector{Connector: base}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE reclaim_fixture(value INTEGER); INSERT INTO reclaim_fixture VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db, dir: dir}, connector
}

func TestWALReclaimDiscardsConnectionWhenRestorationFails(t *testing.T) {
	s, connector := reclaimFailureStore(t)
	connector.failRestore.Store(true)
	if _, err := s.reclaimWAL(context.Background()); !errors.Is(err, errReclaimRestoreFixture) {
		t.Fatal("restoration failure was hidden", err)
	}
	if connector.closed.Load() != 1 || s.ConnectionStats().OpenConnections != 0 {
		t.Fatal("altered connection returned to pool", connector.closed.Load(), s.ConnectionStats())
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal("replacement policy incorrect", timeout, err)
	}
	var value int
	if err := s.db.QueryRow(`SELECT value FROM reclaim_fixture`).Scan(&value); err != nil || value != 1 {
		t.Fatal("committed value lost", value, err)
	}
}

func TestWALReclaimRestoresPolicyAfterMidAttemptCancellation(t *testing.T) {
	s, connector := reclaimFailureStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector.cancelOnZero = cancel
	if ok, err := s.reclaimWAL(ctx); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("mid-attempt cancellation ignored", ok, err)
	}
	if connector.restores.Load() != 1 {
		t.Fatal("cancellation skipped restoration", connector.restores.Load())
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal("canceled attempt leaked policy", timeout, err)
	}
}
