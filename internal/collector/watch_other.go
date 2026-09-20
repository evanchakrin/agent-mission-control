//go:build !windows

package collector

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Portable fallback is bounded but not certified to the native Windows idle
// budget. Above the cap, reconciliation retains coverage without new handles.
type sourceWatcher struct {
	*fsnotify.Watcher
	mu    sync.Mutex
	paths map[string]bool
}

func newSourceWatcher() (*sourceWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &sourceWatcher{Watcher: w, paths: map[string]bool{}}, nil
}
func (w *sourceWatcher) Add(path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	path = filepath.Clean(path)
	if w.paths[path] {
		return nil
	}
	if len(w.paths) >= 4096 {
		return fmt.Errorf("portable directory watch cap reached; periodic reconciliation is required; Windows resource certification does not apply")
	}
	if err := w.Watcher.Add(path); err != nil {
		return err
	}
	w.paths[path] = true
	return nil
}
func (w *sourceWatcher) WatchCount() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.paths) }
