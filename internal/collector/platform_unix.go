//go:build !windows

package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func fileIdentity(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("stable file identity unsupported")
	}
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}
func durableRename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func diskSpace(path string) (uint64, uint64, error) {
	var s syscall.Statfs_t
	err := syscall.Statfs(path, &s)
	return uint64(s.Bavail) * uint64(s.Bsize), uint64(s.Blocks) * uint64(s.Bsize), err
}
