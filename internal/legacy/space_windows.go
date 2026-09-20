//go:build windows

package legacy

import "golang.org/x/sys/windows"

func freeBytes(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, unused uint64
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, &unused)
	return int64(free), err
}
