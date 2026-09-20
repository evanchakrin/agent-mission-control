//go:build windows

package store

import "golang.org/x/sys/windows"

func promoteBlob(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_WRITE_THROUGH)
}

// The evidence file is FlushFileBuffers'd before promotion. Windows does not
// expose POSIX directory fsync through os.File; promotion additionally requests
// MOVEFILE_WRITE_THROUGH before SQLite's FULL commit provides the ledger barrier.
func syncDir(path string) error { return nil }
