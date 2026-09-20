//go:build !windows

package store

import "os"

func promoteBlob(from, to string) error { return os.Rename(from, to) }

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
