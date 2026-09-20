//go:build !windows

package platform

import "os"

func validateReleaseRoot(string) error      { return nil }
func protectReleaseDirectory(string) error  { return nil }
func protectNewConfig(string, Config) error { return nil }
func prepareReleaseRoot(path string) error  { return os.MkdirAll(path, 0755) }
func createReleaseFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
}
func StageServiceConfig(string, Config) error     { return ErrUnsupported }
func createReleaseDirectory(path string) error    { return os.Mkdir(path, 0755) }
func validateServiceMarkerOwnership(string) error { return nil }
