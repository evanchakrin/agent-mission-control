// Package runtimeinfo records bounded operational facts, never transcript,
// credential or arbitrary log text. An unfinished prior run indicates an
// unexpected exit; it cannot reveal whether the cause was a kill, crash or power
// loss. It is independent of the history database and its writer queue.
package runtimeinfo

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const maxJournalBytes = 16 << 10
const ProgressWriteInterval = 30 * time.Second

var (
	ErrLocked  = errors.New("runtime role already running")
	ErrCorrupt = errors.New("runtime journal corrupt")
	ErrClosed  = errors.New("runtime journal finished")
	ErrInvalid = errors.New("invalid runtime journal input")
)
var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type Snapshot struct {
	Schema               int       `json:"schema"`
	Role                 string    `json:"role"`
	Version              string    `json:"version"`
	RunID                string    `json:"runId"`
	Starts               uint64    `json:"starts"`
	Restarts             uint64    `json:"restarts"`
	CountsKnown          bool      `json:"countsKnown"`
	StartedAt            time.Time `json:"startedAt"`
	FinishedAt           time.Time `json:"finishedAt,omitzero"`
	LastDurableProgress  time.Time `json:"lastDurableProgress,omitzero"`
	FinishReason         string    `json:"finishReason,omitempty"`
	PreviousDisposition  string    `json:"previousDisposition"`
	PreviousRunID        string    `json:"previousRunId,omitempty"`
	PreviousStartedAt    time.Time `json:"previousStartedAt,omitzero"`
	PreviousFinishedAt   time.Time `json:"previousFinishedAt,omitzero"`
	PreviousFinishReason string    `json:"previousFinishReason,omitempty"`
	Warning              string    `json:"warning,omitempty"`
}

type Journal struct {
	mu              sync.Mutex
	path            string
	lock            *os.File
	state           Snapshot
	published       atomic.Pointer[Snapshot]
	pendingProgress time.Time
	lastWrite       time.Time
	closed          bool
}

func Begin(dataDir, role, version string) (*Journal, error) {
	if !rolePattern.MatchString(role) || len(version) > 128 || version == "" {
		return nil, ErrInvalid
	}
	for _, r := range version {
		if r < 32 || r > 126 {
			return nil, ErrInvalid
		}
	}
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "runtime")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	for _, path := range []string{base, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, role+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockRole(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	journal := &Journal{path: filepath.Join(dir, role+".json"), lock: lock}
	ok := false
	defer func() {
		if !ok {
			unlockRole(lock)
			lock.Close()
		}
	}()
	prior, readErr := read(journal.path, role)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, ErrCorrupt) {
		return nil, readErr
	}
	countsKnown := true
	starts := uint64(1)
	disposition := "none"
	warning := ""
	if errors.Is(readErr, os.ErrNotExist) {
		if _, err := os.Lstat(journal.path + ".corrupt"); err == nil {
			// Recovery may itself have died after preserving the bad file but
			// before writing the replacement. Do not call that a first-ever run.
			countsKnown = false
			disposition = "unknown-corrupt"
			warning = "Previous journal recovery was interrupted; earlier counts and exit state are unknown"
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if errors.Is(readErr, ErrCorrupt) {
		// Keep one original corrupt file for explicit inspection. If another
		// corrupt file already exists, refuse to erase it or grow an unbounded
		// archive. An operator must resolve the corruption before another run.
		if _, err := os.Lstat(journal.path + ".corrupt"); !errors.Is(err, os.ErrNotExist) {
			return nil, ErrCorrupt
		}
		if err := replace(journal.path, journal.path+".corrupt"); err != nil {
			return nil, err
		}
		countsKnown = false
		disposition = "unknown-corrupt"
		warning = "Previous journal was corrupt; earlier run counts and exit state are unknown"
	} else if readErr == nil {
		starts = prior.Starts + 1
		countsKnown = prior.CountsKnown
		if starts == 0 {
			starts = 1
			countsKnown = false
			warning = "Run count overflow; earlier run counts are unknown"
		}
		disposition = "clean"
		if prior.FinishedAt.IsZero() {
			disposition = "unexpected"
		}
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	journal.state = Snapshot{Schema: 1, Role: role, Version: version, RunID: hex.EncodeToString(random), Starts: starts, Restarts: starts - 1, CountsKnown: countsKnown, StartedAt: now, PreviousDisposition: disposition, Warning: warning}
	if readErr == nil {
		journal.state.LastDurableProgress = prior.LastDurableProgress
		journal.state.PreviousRunID = prior.RunID
		journal.state.PreviousStartedAt = prior.StartedAt
		journal.state.PreviousFinishedAt = prior.FinishedAt
		journal.state.PreviousFinishReason = prior.FinishReason
	}
	if err := journal.persist(journal.state); err != nil {
		return nil, err
	}
	journal.lastWrite = now
	journal.publish()
	ok = true
	return journal, nil
}

func read(path, role string) (Snapshot, error) {
	var result Snapshot
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return result, ErrInvalid
	}
	if info.Size() > maxJournalBytes {
		return result, ErrCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	if err != nil {
		return result, err
	}
	if len(b) > maxJournalBytes || json.Unmarshal(b, &result) != nil {
		return Snapshot{}, ErrCorrupt
	}
	if result.Schema > 1 {
		return Snapshot{}, errors.New("runtime journal schema is newer than this binary")
	}
	if result.Schema != 1 || result.Role != role || result.Starts == 0 || result.Restarts != result.Starts-1 || result.RunID == "" || result.StartedAt.IsZero() || (!result.FinishedAt.IsZero() && (result.FinishedAt.Before(result.StartedAt) || !safeReason(result.FinishReason))) {
		return Snapshot{}, ErrCorrupt
	}
	return result, nil
}

// Health reads never wait behind journal fsync/rename. The published value is
// immutable and replaced only after persistence succeeds; it is not pending
// progress or an assertion that storage is currently writable.
func (j *Journal) Snapshot() Snapshot {
	if value := j.published.Load(); value != nil {
		return *value
	}
	return Snapshot{}
}

// Caller owns the writer lock (or the journal has not yet escaped Begin).
func (j *Journal) publish() {
	value := j.state
	j.published.Store(&value)
}

// NoteProgress is monotonic and write-coalesced to at most once per 30 seconds.
// Snapshot reports only successfully persisted progress; Finish flushes the last
// pending timestamp. A crash may lose at most this progress interval, not history.
func (j *Journal) NoteProgress(at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	if at.IsZero() || at.After(time.Now().Add(time.Minute)) {
		return ErrInvalid
	}
	at = at.UTC()
	if at.After(j.pendingProgress) {
		j.pendingProgress = at
	}
	if !j.pendingProgress.After(j.state.LastDurableProgress) {
		return nil
	}
	if !j.state.LastDurableProgress.IsZero() && time.Since(j.lastWrite) < ProgressWriteInterval {
		return nil
	}
	next := j.state
	next.LastDurableProgress = j.pendingProgress
	if err := j.persist(next); err != nil {
		return err
	}
	j.state = next
	j.lastWrite = time.Now()
	j.publish()
	return nil
}

func safeReason(reason string) bool {
	switch reason {
	case "clean", "shutdown", "signal", "context-canceled", "startup-error", "runtime-error", "stopped", "completed", "error":
		return true
	}
	return false
}

func (j *Journal) Finish(reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	if !safeReason(reason) {
		return ErrInvalid
	}
	next := j.state
	next.FinishedAt = time.Now().UTC()
	next.FinishReason = reason
	if j.pendingProgress.After(next.LastDurableProgress) {
		next.LastDurableProgress = j.pendingProgress
	}
	if err := j.persist(next); err != nil {
		return err
	}
	j.state = next
	j.closed = true
	j.publish()
	err := unlockRole(j.lock)
	closeErr := j.lock.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (j *Journal) persist(value Snapshot) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxJournalBytes {
		return ErrInvalid
	}
	// One fixed staging slot per locked role bounds files even after repeated
	// hard kills between fsync and rename. Refuse links/nonregular objects.
	temp := j.path + ".pending"
	if info, err := os.Lstat(temp); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return ErrInvalid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(b)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return replace(temp, j.path)
}
