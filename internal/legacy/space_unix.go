//go:build !windows

package legacy

import "golang.org/x/sys/unix"

func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	err := unix.Statfs(path, &st)
	return int64(st.Bavail) * int64(st.Bsize), err
}
