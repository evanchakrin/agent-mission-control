//go:build !windows

package desktop

import (
	"errors"
	"os"
	"syscall"
)

func platformLinkCheck(_ string, st os.FileInfo, isTarget bool) error {
	if isTarget && !st.IsDir() {
		if stat, ok := st.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
			return errors.New("hard-linked files are not editable here")
		}
	}
	return nil
}
