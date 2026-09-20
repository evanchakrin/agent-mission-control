package platform

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const LogMaxBytes int64 = 10 * 1024 * 1024
const LogMaxFiles = 5 // active file plus four previous files

func NewJSONLogger(filename string) (*slog.Logger, io.Closer, error) {
	w, err := newRotatingWriter(filename, LogMaxBytes, LogMaxFiles)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})), w, nil
}

type rotatingWriter struct {
	mu             sync.Mutex
	filename       string
	file           *os.File
	size, maxBytes int64
	maxFiles       int
	closed         bool
}

func newRotatingWriter(filename string, maxBytes int64, maxFiles int) (*rotatingWriter, error) {
	if !filepath.IsAbs(filename) || maxBytes < 1 || maxFiles < 1 {
		return nil, errors.New("invalid log path or rotation limits")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return nil, err
	}
	w := &rotatingWriter{filename: filename, maxBytes: maxBytes, maxFiles: maxFiles}
	return w, w.open()
}
func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file, w.size = f, st.Size()
	return nil
}
func (w *rotatingWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	last := w.filename
	if w.maxFiles > 1 {
		last += "." + strconv.Itoa(w.maxFiles-1)
	}
	if err := os.Remove(last); err != nil && !os.IsNotExist(err) {
		return err
	}
	for n := w.maxFiles - 2; n >= 0; n-- {
		from := w.filename
		if n > 0 {
			from += "." + strconv.Itoa(n)
		}
		if err := os.Rename(from, w.filename+"."+strconv.Itoa(n+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return w.open()
}
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	// Keep even malformed/oversize logging calls from defeating the disk cap.
	if int64(len(p)) > w.maxBytes {
		return 0, errors.New("log record exceeds rotation limit")
	}
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	if w.file == nil {
		return 0, errors.New("log file is unavailable after rotation failure")
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}
