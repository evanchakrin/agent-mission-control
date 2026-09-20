//go:build windows

package platform

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func validateReleaseRoot(root string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("staging a service release requires administrator access")
	}
	_, err := releaseAnchor(root)
	return err
}
func protectReleaseDirectory(dir string) error {
	return setAdministratorACL(dir, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)")
}

func prepareReleaseRoot(root string) error {
	base, err := releaseAnchor(root)
	if err != nil {
		return err
	}
	return ensureProtectedDirectoryTree(base, root)
}
func createReleaseFile(filename string) (*os.File, error) {
	return createAdministratorFile(filename, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)")
}
func createReleaseDirectory(path string) error { return createAdministratorDirectory(path) }
func verifyReleaseFile(filename string) error {
	base, err := releaseAnchor(filepath.Dir(filename))
	if err != nil {
		return err
	}
	return verifyProtectedPath(base, filename)
}
func protectNewConfig(filename string, c Config) error {
	if c.Role == Desktop {
		return applyDACL(filename, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+c.OwnerSID+")")
	}
	// The explicit owner must be able to publish and remove the staging hard
	// link without elevation. Installation subsequently seals this file to
	// owner-read and grants precisely the virtual service's read permission.
	// No other non-administrator principal can read the staged credential.
	return applyDACL(filename, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+c.OwnerSID+")")
}
