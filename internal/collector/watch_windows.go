//go:build windows

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

const nativeWatchBufferBytes = 64 * 1024
const nativeWatchRootsLimit = 64

// Windows recursive notifications have one fixed-size buffer and filesystem
// handle per root, not per transcript/session/subagent directory. Each root
// also has an overlapped-completion event; all roots share one shutdown event.
type sourceWatcher struct {
	Events  chan fsnotify.Event
	Errors  chan error
	mu      sync.Mutex
	roots   map[string]*rootWatch
	stop    windows.Handle
	done    chan struct{}
	closed  chan struct{}
	closing bool
	wg      sync.WaitGroup
}
type rootWatch struct {
	path          string
	handle, event windows.Handle
	overlap       windows.Overlapped
	buffer        []byte
	pin           runtime.Pinner
	identity      [3]uint32
	ioMu          sync.Mutex
	retiring      bool
}

func newSourceWatcher() (*sourceWatcher, error) {
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	return &sourceWatcher{Events: make(chan fsnotify.Event, 256), Errors: make(chan error, 1), roots: map[string]*rootWatch{}, stop: stop, done: make(chan struct{}), closed: make(chan struct{})}, nil
}
func watchPathWithin(root, path string) bool {
	rel, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (w *sourceWatcher) Add(path string) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return errors.New("recursive notification roots must be absolute local directories")
	}
	path = filepath.Clean(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return fsnotify.ErrClosed
	}
	var existing *rootWatch
	for _, root := range w.roots {
		if strings.EqualFold(root.path, path) {
			existing = root
			break
		}
		if watchPathWithin(root.path, path) {
			return nil
		}
	}
	if existing == nil && len(w.roots) >= nativeWatchRootsLimit {
		return fmt.Errorf("recursive watcher root limit reached (%d); periodic reconciliation remains available", nativeWatchRootsLimit)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("watch root must be a real directory")
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("watch root must not be a reparse point")
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return err
	}
	var fileInfo windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &fileInfo); err != nil {
		windows.CloseHandle(h)
		return err
	}
	identity := [3]uint32{fileInfo.VolumeSerialNumber, fileInfo.FileIndexHigh, fileInfo.FileIndexLow}
	if existing != nil {
		windows.CloseHandle(h)
		if existing.identity == identity {
			return nil
		}
		// A watched root can be renamed and replaced without its old handle
		// becoming invalid. Retire it and request a fresh reconcile/watch so
		// future events do not remain attached to the moved-away directory.
		existing.ioMu.Lock()
		existing.retiring = true
		_ = windows.CancelIoEx(existing.handle, &existing.overlap)
		existing.ioMu.Unlock()
		w.signalError(fsnotify.ErrEventOverflow)
		return errors.New("source root identity changed; notification watch is restarting")
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return err
	}
	r := &rootWatch{path: path, handle: h, event: event, buffer: make([]byte, nativeWatchBufferBytes), identity: identity}
	r.pin.Pin(&r.buffer[0])
	r.pin.Pin(&r.overlap)
	if err = r.arm(); err != nil {
		r.pin.Unpin()
		windows.CloseHandle(event)
		windows.CloseHandle(h)
		return err
	}
	w.roots[strings.ToLower(path)] = r
	w.wg.Add(1)
	go w.readRoot(r)
	return nil
}
func (r *rootWatch) arm() error {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	if r.retiring {
		return fsnotify.ErrClosed
	}
	if err := windows.ResetEvent(r.event); err != nil {
		return err
	}
	r.overlap = windows.Overlapped{HEvent: r.event}
	const mask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME | windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE
	return windows.ReadDirectoryChanges(r.handle, &r.buffer[0], uint32(len(r.buffer)), true, mask, nil, &r.overlap, 0)
}
func (w *sourceWatcher) signalError(err error) {
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.Errors <- err:
	default:
	}
}
func (w *sourceWatcher) emit(event fsnotify.Event) {
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.Events <- event:
	default:
		w.signalError(fsnotify.ErrEventOverflow)
	}
}
func (w *sourceWatcher) readRoot(r *rootWatch) {
	defer func() {
		// All outstanding IO has completed before its buffer/OVERLAPPED is
		// unpinned or freed, including the CancelIoEx path.
		w.mu.Lock()
		delete(w.roots, strings.ToLower(r.path))
		w.mu.Unlock()
		r.pin.Unpin()
		windows.CloseHandle(r.event)
		windows.CloseHandle(r.handle)
		w.wg.Done()
	}()
	for {
		which, err := windows.WaitForMultipleObjects([]windows.Handle{w.stop, r.event}, false, windows.INFINITE)
		var transferred uint32
		if err != nil || which == windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(r.handle, &r.overlap)
			// Microsoft requires waiting for cancellation completion before
			// reusing/finalizing the OVERLAPPED or notification buffer.
			_ = windows.GetOverlappedResult(r.handle, &r.overlap, &transferred, true)
			if err != nil {
				w.signalError(err)
			}
			return
		}
		err = windows.GetOverlappedResult(r.handle, &r.overlap, &transferred, false)
		if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) || (err == nil && transferred == 0) {
			w.signalError(fsnotify.ErrEventOverflow)
		} else if err != nil {
			w.signalError(fmt.Errorf("recursive source notifications unavailable: %w", err))
			return
		} else if transferred > uint32(len(r.buffer)) {
			w.signalError(fsnotify.ErrEventOverflow)
		} else if err = parseNativeChanges(r.path, r.buffer[:transferred], w.emit); err != nil {
			w.signalError(err)
		}
		select {
		case <-w.done:
			return
		default:
		}
		if err = r.arm(); err != nil {
			w.signalError(err)
			return
		}
	}
}

// FILE_NOTIFY_INFORMATION is a linked list of DWORD-aligned records. Validate
// every length/offset before decoding; overflow never means history is deleted.
func parseNativeChanges(root string, data []byte, emit func(fsnotify.Event)) error {
	for offset := 0; ; {
		if len(data)-offset < 12 {
			return fsnotify.ErrEventOverflow
		}
		r := data[offset:]
		next := int(binary.LittleEndian.Uint32(r[:4]))
		action := binary.LittleEndian.Uint32(r[4:8])
		length := int(binary.LittleEndian.Uint32(r[8:12]))
		if length%2 != 0 || length > len(r)-12 {
			return fsnotify.ErrEventOverflow
		}
		name16 := make([]uint16, length/2)
		for i := range name16 {
			name16[i] = binary.LittleEndian.Uint16(r[12+i*2 : 14+i*2])
		}
		name := string(utf16.Decode(name16))
		path := filepath.Join(root, name)
		if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, "\x00:") || !watchPathWithin(root, path) {
			return fsnotify.ErrEventOverflow
		}
		var op fsnotify.Op
		switch action {
		case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
			op = fsnotify.Create
		case windows.FILE_ACTION_REMOVED:
			op = fsnotify.Remove
		case windows.FILE_ACTION_MODIFIED:
			op = fsnotify.Write
		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			op = fsnotify.Rename
		default:
			return fsnotify.ErrEventOverflow
		}
		emit(fsnotify.Event{Name: path, Op: op})
		if next == 0 {
			return nil
		}
		if next%4 != 0 || next < 12+length || next >= len(r) {
			return fsnotify.ErrEventOverflow
		}
		offset += next
	}
}
func (w *sourceWatcher) WatchCount() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.roots) }
func (w *sourceWatcher) Close() error {
	w.mu.Lock()
	if !w.closing {
		w.closing = true
		close(w.done)
		_ = windows.SetEvent(w.stop)
		go func() { w.wg.Wait(); windows.CloseHandle(w.stop); close(w.Events); close(w.Errors); close(w.closed) }()
	}
	w.mu.Unlock()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-w.closed:
		return nil
	case <-timer.C:
		return errors.New("filesystem notification cancellation exceeded 3 seconds; pending IO buffers remain pinned until completion")
	}
}
