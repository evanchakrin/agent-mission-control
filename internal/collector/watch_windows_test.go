//go:build windows

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

func TestRecursiveWatcherUsesConstantRootCountAndSeesDeepChanges(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1500; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("session-%04d", i), "subagents"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	w, err := newSourceWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err = w.Add(root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		if err = w.Add(filepath.Join(root, fmt.Sprintf("session-%04d", i), "subagents")); err != nil {
			t.Fatal(err)
		}
	}
	if count := w.WatchCount(); count != 1 {
		t.Fatalf("nested folders created %d watches", count)
	}
	// Future descendants are already covered by recursive notification. Even
	// a 100k-session catalog cannot accumulate path registrations or buffers.
	for i := 0; i < 100000; i++ {
		if err = w.Add(filepath.Join(root, fmt.Sprintf("future-session-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if count := w.WatchCount(); count != 1 {
		t.Fatalf("100k descendant registrations retained %d root watches", count)
	}
	path := filepath.Join(root, "session-1499", "subagents", "rollout.jsonl")
	if err = os.WriteFile(path, []byte("{\"hello\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	found := false
	for !found {
		select {
		case e := <-w.Events:
			found = e.Name == path && e.Op&(fsnotify.Create|fsnotify.Write) != 0
		case err := <-w.Errors:
			t.Fatalf("unexpected native watcher error: %v", err)
		case <-timer.C:
			t.Fatal("recursive watcher missed a deep transcript change")
		}
	}
	start := time.Now()
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("idle overlapped cancellation was too slow")
	}
	if w.WatchCount() != 0 {
		t.Fatal("handles survived close")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Add(root); !errors.Is(err, fsnotify.ErrClosed) {
		t.Fatalf("closed watcher accepted Add: %v", err)
	}
}

func TestRecursiveWatcherReportsQueueOverflowWithoutBlocking(t *testing.T) {
	w, err := newSourceWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < cap(w.Events)+20; i++ {
		w.emit(fsnotify.Event{Name: "file", Op: fsnotify.Write})
	}
	select {
	case err := <-w.Errors:
		if !errors.Is(err, fsnotify.ErrEventOverflow) {
			t.Fatalf("wrong overflow: %v", err)
		}
	default:
		t.Fatal("event loss did not request reconciliation")
	}
}

func TestNativeNotificationDecoderRejectsMalformedRangesAndEscapes(t *testing.T) {
	root := t.TempDir()
	packet := func(name string) []byte {
		u := utf16.Encode([]rune(name))
		b := make([]byte, 12+2*len(u))
		binary.LittleEndian.PutUint32(b[4:8], windows.FILE_ACTION_ADDED)
		binary.LittleEndian.PutUint32(b[8:12], uint32(2*len(u)))
		for i, v := range u {
			binary.LittleEndian.PutUint16(b[12+i*2:14+i*2], v)
		}
		return b
	}
	var got fsnotify.Event
	if err := parseNativeChanges(root, packet("deep\\rollout.jsonl"), func(e fsnotify.Event) { got = e }); err != nil || got.Name != filepath.Join(root, "deep", "rollout.jsonl") {
		t.Fatalf("valid notification: %+v %v", got, err)
	}
	for _, data := range [][]byte{{}, make([]byte, 11), packet("..\\outside.jsonl"), packet("C:\\outside.jsonl")} {
		if err := parseNativeChanges(root, data, func(fsnotify.Event) {}); err == nil {
			t.Fatal("unsafe notification accepted")
		}
	}
	bad := packet("file")
	binary.LittleEndian.PutUint32(bad[:4], 1)
	if err := parseNativeChanges(root, bad, func(fsnotify.Event) {}); err == nil {
		t.Fatal("malformed next offset accepted")
	}
}

func TestRecursiveWatcherRepeatedPendingCancel(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		w, err := newSourceWatcher()
		if err != nil {
			t.Fatal(err)
		}
		if err = w.Add(root); err != nil {
			w.Close()
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecursiveWatcherReattachesWhenRootDirectoryIsReplaced(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "transcripts")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	w, err := newSourceWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.Add(root); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(root, filepath.Join(base, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = w.Add(root); err == nil {
		t.Fatal("replacement root was mistaken for the watched directory")
	}
	deadline := time.Now().Add(2 * time.Second)
	for w.WatchCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.WatchCount() != 0 {
		t.Fatal("replaced directory watch did not cancel")
	}
	if err = w.Add(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "new-history.jsonl")
	if err = os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-w.Events:
			if e.Name == path {
				return
			}
		case <-w.Errors: // Replacement intentionally requests reconciliation.
		case <-timer.C:
			t.Fatal("watch stayed attached to the moved-away root")
		}
	}
}
