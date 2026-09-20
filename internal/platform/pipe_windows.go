//go:build windows

package platform

import (
	"context"
	"errors"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"net"
)

func localPipePath(name string) (string, error) {
	if !pipeNamePattern.MatchString(name) {
		return "", errors.New("pipe name must be a local AMC identifier")
	}
	return `\\.\pipe\AMC-` + name, nil
}

func ListenOwnerPipe(name, ownerSID string) (net.Listener, error) {
	p, err := localPipePath(name)
	if err != nil {
		return nil, err
	}
	if !ownerSIDPattern.MatchString(ownerSID) {
		return nil, errors.New("pipe owner must be an individual Windows user SID")
	}
	sid, err := windows.StringToSid(ownerSID)
	if err != nil {
		return nil, err
	}
	// Listener's current identity also needs permission to create instances.
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + tokenUser.User.Sid.String() + ")(A;;GRGW;;;" + sid.String() + ")"
	// go-winio v0.6.2 always creates listeners with FILE_PIPE_REJECT_REMOTE_CLIENTS.
	return winio.ListenPipe(p, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 64 * 1024, OutputBufferSize: 64 * 1024})
}
func DialOwnerPipe(ctx context.Context, name string) (net.Conn, error) {
	p, err := localPipePath(name)
	if err != nil {
		return nil, err
	}
	return winio.DialPipeAccessImpLevel(ctx, p, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

func CurrentOwnerSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}
