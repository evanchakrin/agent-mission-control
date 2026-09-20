//go:build windows

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"sync"
)

type instanceLock struct {
	handle windows.Handle
	once   sync.Once
	err    error
}

func (l *instanceLock) Close() error {
	l.once.Do(func() { l.err = windows.CloseHandle(l.handle) })
	return l.err
}
func AcquireInstance(role Role, dataDir string) (io.Closer, error) {
	if !role.valid() {
		return nil, errors.New("invalid role")
	}
	dir, err := safeAbsolutePath(dataDir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(string(role) + ":" + pathKey(dir)))
	name, _ := windows.UTF16PtrFromString(`Global\AMC-` + hex.EncodeToString(sum[:16]))
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, err
	}
	return &instanceLock{handle: h}, nil
}
