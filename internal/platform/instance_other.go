//go:build !windows

package platform

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type instanceLock struct{ file *os.File }

func (l *instanceLock) Close() error { return l.file.Close() }
func AcquireInstance(role Role, dataDir string) (io.Closer, error) {
	if !role.valid() {
		return nil, errors.New("invalid role")
	}
	dir, err := safeAbsolutePath(dataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".amc-"+string(role)+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &instanceLock{file: f}, nil
}
