package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const serviceDataMarker = ".amc-service-data.json"

type dataOwnership struct {
	Version  int    `json:"version"`
	Role     Role   `json:"role"`
	OwnerSID string `json:"ownerSid"`
}

// Installation may replace an ACL only on a dedicated new/empty directory or
// state already marked for this same AMC role and owner. A typo must not replace
// the DACL on an existing profile, repository or unrelated data directory.
func validateServiceData(c Config) error {
	root, err := safeAbsolutePath(c.DataDir)
	if err != nil {
		return err
	}
	if pathKey(root) != pathKey(c.DataDir) {
		return errors.New("service data must not pass through a junction or symbolic link")
	}
	for _, broad := range []string{os.Getenv("USERPROFILE"), os.Getenv("ProgramData"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("SystemRoot")} {
		if broad != "" && within(root, broad) {
			return errors.New("service data directory is too broad")
		}
	}
	for _, system := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("SystemRoot")} {
		if system != "" && within(system, root) {
			return errors.New("service data must not be inside a system or executable directory")
		}
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	marker := filepath.Join(root, serviceDataMarker)
	resolved, err := safeAbsolutePath(marker)
	if err != nil || pathKey(resolved) != pathKey(marker) {
		return errors.New("invalid service data ownership marker")
	}
	f, err := os.Open(marker)
	if err != nil {
		return errors.New("service data is not empty and lacks an AMC ownership marker; refusing to alter its ACL")
	}
	defer f.Close()
	var ownership dataOwnership
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	if err = d.Decode(&ownership); err != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid service data ownership marker")
	}
	if ownership.Version != 1 || ownership.Role != c.Role || ownership.OwnerSID != c.OwnerSID {
		return errors.New("service data belongs to another role or owner")
	}
	return validateServiceMarkerOwnership(marker)
}
func markServiceData(c Config) error {
	path := filepath.Join(c.DataDir, serviceDataMarker)
	if _, err := os.Lstat(path); err == nil {
		return validateServiceData(c)
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(dataOwnership{Version: 1, Role: c.Role, OwnerSID: c.OwnerSID})
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("write service ownership marker: %w", err)
	}
	return closeErr
}
