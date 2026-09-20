//go:build !windows

package platform

import "syscall"

func FreeSpace(path string) (availableBytes, totalBytes uint64, err error) {
	var st syscall.Statfs_t
	err = syscall.Statfs(path, &st)
	if err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
