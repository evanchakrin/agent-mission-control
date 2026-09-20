package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var releaseVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

type Release struct {
	Executable string `json:"executable"`
	SHA256     string `json:"sha256"`
}

// StageRelease makes a new immutable version directory. It never replaces a
// previous release, changes a running service, or removes a failed staging dir.
func StageRelease(sourceExecutable, releaseRoot, version string) (Release, error) {
	var result Release
	if !releaseVersion.MatchString(version) || version == "." || version == ".." {
		return result, errors.New("invalid release version")
	}
	source, err := safeAbsolutePath(sourceExecutable)
	if err != nil {
		return result, err
	}
	root, err := safeAbsolutePath(releaseRoot)
	if err != nil {
		return result, err
	}
	if pathKey(root) != pathKey(releaseRoot) {
		return result, errors.New("release destination must not pass through a junction or symlink")
	}
	if err = validateReleaseRoot(root); err != nil {
		return result, err
	}
	dir := filepath.Join(root, version)
	if within(dir, source) {
		return result, errors.New("release source is inside the destination")
	}
	st, err := os.Stat(source)
	if err != nil || !st.Mode().IsRegular() {
		return result, errors.New("release source must be an existing regular executable")
	}
	if err = prepareReleaseRoot(root); err != nil {
		return result, err
	}
	if err = createReleaseDirectory(dir); err != nil {
		return result, fmt.Errorf("release version must be new: %w", err)
	}
	if err = protectReleaseDirectory(dir); err != nil {
		return result, err
	}
	in, err := os.Open(source)
	if err != nil {
		return result, err
	}
	defer in.Close()
	dest := filepath.Join(dir, "amc.exe")
	out, err := createReleaseFile(dest)
	if err != nil {
		return result, err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return result, copyErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	return Release{Executable: dest, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// WriteProtectedConfig creates a new config; replacement is an explicit release
// operation, not a side effect of install. A temporary file is protected before
// the credential is written, then renamed within the destination directory.
func WriteProtectedConfig(filename string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	p, err := safeAbsolutePath(filename)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(p); err == nil {
		return errors.New("configuration already exists; refusing to overwrite it")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".amc-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = protectNewConfig(tmp, c); err != nil {
		f.Close()
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Link gives us an atomic no-replace publication on supported local filesystems.
	if err = os.Link(tmp, p); err != nil {
		return fmt.Errorf("publish new protected configuration: %w", err)
	}
	return nil
}
