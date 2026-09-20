//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// Evidence can contain private operational details. Only Administrators and
// SYSTEM receive access, unlike the executable's read/execute permissions.
func StageReleaseEvidence(manifestPath, executable string) (string, error) {
	if err := validateReleaseRoot(filepath.Dir(executable)); err != nil {
		return "", err
	}
	if err := verifyReleaseFile(executable); err != nil {
		return "", err
	}
	return stageReleaseEvidence(manifestPath, executable, evidenceStageFiles{
		directory: func(path string) error {
			return createAdministratorDirectorySDDL(path, "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
		},
		file: func(path string) (*os.File, error) {
			return createAdministratorFile(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
		},
	})
}
