package platform

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

type DesktopSpec struct {
	Executable string
	ConfigPath string
	OwnerSID   string
}

var ownerSIDPattern = regexp.MustCompile(`^S-1-(?:5-21|12-1)-(?:[0-9]+-){3}[0-9]+$`)

func (s DesktopSpec) Validate() error {
	if !ownerSIDPattern.MatchString(s.OwnerSID) {
		return errors.New("desktop task requires the individual Windows user's SID")
	}
	for _, p := range []string{s.Executable, s.ConfigPath} {
		if _, err := safeAbsolutePath(p); err != nil {
			return err
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return errors.New("desktop executable and configuration must exist")
		}
	}
	return nil
}
func DesktopTaskName(ownerSID string) (string, error) {
	if !ownerSIDPattern.MatchString(ownerSID) {
		return "", errors.New("invalid desktop owner SID")
	}
	return `AMC Desktop ` + ownerSID, nil
}
func quoteWindowsArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		if r == '\\' {
			slashes++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat("\\", slashes*2+1))
		} else {
			b.WriteString(strings.Repeat("\\", slashes))
		}
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat("\\", slashes*2))
	b.WriteByte('"')
	return b.String()
}
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// schtasks reads XML files through Windows' encoding detection. Supply a BOM
// and a matching declaration instead of letting it interpret UTF-8 as UTF-16.
func desktopTaskFileBytes(data []byte) []byte {
	s := strings.Replace(string(data), `encoding="UTF-8"`, `encoding="UTF-16"`, 1)
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(units)*2)
	out[0], out[1] = 0xff, 0xfe
	for i, u := range units {
		out[2+i*2], out[3+i*2] = byte(u), byte(u>>8)
	}
	return out
}

// DesktopTaskXML is a pure, reviewable task definition. It requires the user's
// existing interactive token, never a stored password or elevated privileges.
func DesktopTaskXML(spec DesktopSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
 <RegistrationInfo><Description>Agent Mission Control local desktop interface. Collection runs separately as a Windows service.</Description></RegistrationInfo>
 <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers>
 <Principals><Principal id="Owner"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
 <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>true</StartWhenAvailable><RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><Enabled>true</Enabled><Hidden>true</Hidden><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure></Settings>
 <Actions Context="Owner"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>
`, xmlText(spec.OwnerSID), xmlText(spec.OwnerSID), xmlText(spec.Executable), xmlText("desktop --config "+quoteWindowsArg(spec.ConfigPath)), xmlText(filepath.Dir(spec.Executable)))), nil
}
