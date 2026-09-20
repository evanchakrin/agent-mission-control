package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/evanchakrin/agent-mission-control/internal/desktop"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

// importOwner is offline maintenance, not a network-accessible file operation.
// It shares the desktop instance lock and the existing resumable import rules.
func importOwner(c platform.Config, source string) (map[string]int, error) {
	if c.Role != platform.Desktop {
		return nil, errors.New("import-owner requires a desktop configuration")
	}
	if !filepath.IsAbs(source) || strings.HasPrefix(source, `\\`) || strings.ContainsAny(source, "\x00\r\n") {
		return nil, errors.New("import-owner requires --source with an absolute local legacy directory")
	}
	if c.OwnerHomeDir == "" {
		return nil, errors.New("import-owner requires an explicit ownerHomeDir")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := platform.ValidateDesktopIdentity(c.OwnerSID); err != nil {
		return nil, err
	}
	lock, err := platform.AcquireInstance(platform.Desktop, c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("stop the desktop before owner import: %w", err)
	}
	defer lock.Close()
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	m, err := desktop.New(desktop.Options{StateDir: c.DataDir, HomeDir: c.OwnerHomeDir,
		LocalMachineID: c.MachineID, Roots: c.RepositoryRoots, CSRFToken: hex.EncodeToString(token[:])})
	if err != nil {
		return nil, err
	}
	defer m.Close()
	return m.ImportLegacy(source)
}
