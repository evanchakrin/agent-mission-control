//go:build windows

package platform

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const administratorsSID = "S-1-5-32-544"
const systemSID = "S-1-5-18"
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func trustedInstallationSID(sid string) bool {
	return sid == administratorsSID || sid == systemSID || sid == trustedInstallerSID
}

// Known-folder APIs, not caller-controlled environment variables, establish
// production anchors. Only AMC's own application subtree can be staged.
func releaseAnchor(path string) (string, error) {
	for _, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_ProgramFiles, windows.FOLDERID_ProgramFilesX86} {
		base, err := windows.KnownFolderPath(id, windows.KF_FLAG_DEFAULT)
		if err != nil {
			continue
		}
		app := filepath.Join(base, "AgentMissionControl")
		if within(app, path) {
			return base, nil
		}
	}
	return "", errors.New("service releases must stay in the OS Program Files\\AgentMissionControl subtree")
}
func configAnchor(path string) (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	if !within(filepath.Join(base, "AgentMissionControl", "config"), filepath.Dir(path)) {
		return "", errors.New("production configuration must stay in OS ProgramData\\AgentMissionControl\\config")
	}
	return base, nil
}
func rejectPathReparse(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("protected installation paths cannot contain reparse points")
	}
	return nil
}

// An object owner can rewrite its DACL even if an explicit ACE looks read-only.
// Likewise DELETE_CHILD on any parent can replace a sealed child. Treat unknown
// ACE forms as unsupported instead of trying to interpret them permissively.
func verifyInstallationDescriptor(sd *windows.SECURITY_DESCRIPTOR, anchor bool) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !trustedInstallationSID(owner.String()) {
		return errors.New("installation object is not owned by Administrators, SYSTEM or TrustedInstaller")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("installation object has a missing/null DACL")
	}
	const deleteChild = 0x40
	mask := uint32(windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE | deleteChild)
	if !anchor {
		mask |= windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported installation access-control entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if uint32(ace.Mask)&mask != 0 && !trustedInstallationSID(sid.String()) {
			return fmt.Errorf("installation path permits non-administrator replacement or writes (%s)", sid.String())
		}
	}
	return nil
}
func verifyInstallationObject(path string, anchor bool) error {
	if err := rejectPathReparse(path); err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if err = verifyInstallationDescriptor(sd, anchor); err != nil {
		return fmt.Errorf("unprotected installation path %s: %w", path, err)
	}
	return nil
}
func verifyAnchorParents(base string) error {
	for p := filepath.Clean(base); ; p = filepath.Dir(p) {
		if err := verifyInstallationObject(p, true); err != nil {
			return err
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}
func directoryChain(base, target string) ([]string, error) {
	if !filepath.IsAbs(base) || !filepath.IsAbs(target) || !within(base, target) {
		return nil, errors.New("protected directory must stay beneath its trusted anchor")
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, nil
	}
	var out []string
	current := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		out = append(out, current)
	}
	return out, nil
}
func ensureProtectedDirectoryTree(base, target string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("production staging requires an elevated administrator token")
	}
	if err := verifyAnchorParents(base); err != nil {
		return err
	}
	paths, err := directoryChain(base, target)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err = createAdministratorDirectory(path); err != nil && !os.IsExist(err) {
			return err
		}
		// Never repair an existing untrusted directory implicitly: it may
		// contain unrelated data, child ACLs or preexisting writable handles.
		if err = verifyInstallationObject(path, false); err != nil {
			return err
		}
	}
	return nil
}

func createAdministratorDirectory(path string) error {
	return createAdministratorDirectorySDDL(path, "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)")
}

func createAdministratorDirectorySDDL(path, sddl string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("production directory staging requires administrator elevation")
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(p, &attrs)
}

func prepareServiceDataParent(c Config) error {
	parent := filepath.Dir(c.DataDir)
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return err
	}
	if within(filepath.Join(base, "AgentMissionControl"), parent) {
		return ensureProtectedDirectoryTree(base, parent)
	}
	// Custom volumes are supported only when the operator has already created
	// a protected parent. Never re-ACL a broader, user-selected directory.
	if err = verifyAnchorParents(parent); err != nil {
		return err
	}
	return verifyInstallationObject(parent, false)
}

func validateServiceMarkerOwnership(path string) error { return verifyInstallationObject(path, false) }
func verifyProtectedPath(base, path string) error {
	if err := verifyAnchorParents(base); err != nil {
		return err
	}
	paths, err := directoryChain(base, path)
	if err != nil {
		return err
	}
	for _, part := range paths {
		if err = verifyInstallationObject(part, false); err != nil {
			return err
		}
	}
	return nil
}
func setAdministratorACL(path, sddl string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("sealing production ownership requires administrator elevation")
	}
	sd, err := windows.SecurityDescriptorFromString("O:BA" + sddl)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
}
func createAdministratorFile(path, sddl string) (*os.File, error) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return nil, errors.New("production file staging requires administrator elevation")
	}
	sd, err := windows.SecurityDescriptorFromString("O:BA" + sddl)
	if err != nil {
		return nil, err
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// StageServiceConfig is distinct from private candidate/backup configuration.
// It creates protected Administrator-owned parents and a sealed configuration
// atomically, without a window granting the desktop owner write access.
func StageServiceConfig(filename string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Role.ServiceName() == "" {
		return errors.New("desktop configuration is owned by the unelevated user, not staged as service configuration")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("production configuration staging requires administrator elevation")
	}
	p, err := safeAbsolutePath(filename)
	if err != nil {
		return err
	}
	if pathKey(p) != pathKey(filename) {
		return errors.New("production configuration must not pass through a junction or symlink")
	}
	base, err := configAnchor(p)
	if err != nil {
		return err
	}
	if err = ensureProtectedDirectoryTree(base, filepath.Dir(p)); err != nil {
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(p), ".amc-stage-"+hex.EncodeToString(id[:]))
	f, err := createAdministratorFile(tmp, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;"+c.OwnerSID+")")
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	err = json.NewEncoder(f).Encode(c)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(tmp, p); err != nil {
		return err
	}
	return verifyProtectedPath(base, p)
}
func verifyServiceConfig(filename string) error {
	base, err := configAnchor(filename)
	if err != nil {
		return err
	}
	return verifyProtectedPath(base, filename)
}
