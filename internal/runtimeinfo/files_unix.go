//go:build !windows

package runtimeinfo

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lockRole(f *os.File) error   { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockRole(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func replace(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
