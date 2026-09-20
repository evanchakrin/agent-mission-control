//go:build windows

package desktop

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func platformLinkCheck(path string, st os.FileInfo, isTarget bool) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse-point files and folders are not editable here")
	}
	if isTarget && !st.IsDir() {
		h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		var info windows.ByHandleFileInformation
		if err = windows.GetFileInformationByHandle(h, &info); err != nil {
			return err
		}
		if info.NumberOfLinks > 1 {
			return errors.New("hard-linked files are not editable here")
		}
	}
	return nil
}
