//go:build !windows

package migration

import "golang.org/x/sys/unix"

func freeBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	return int64(stat.Bavail) * int64(stat.Bsize), err
}
