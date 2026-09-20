//go:build windows

package platform

import "golang.org/x/sys/windows"

func FreeSpace(path string) (availableBytes, totalBytes uint64, err error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, 0, e
	}
	var totalFree uint64
	err = windows.GetDiskFreeSpaceEx(p, &availableBytes, &totalBytes, &totalFree)
	return
}
