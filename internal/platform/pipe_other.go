//go:build !windows

package platform

import (
	"context"
	"net"
)

// There is deliberately no insecure TCP substitute for authenticated local IPC.
func ListenOwnerPipe(string, string) (net.Listener, error)    { return nil, ErrUnsupported }
func DialOwnerPipe(context.Context, string) (net.Conn, error) { return nil, ErrUnsupported }
func CurrentOwnerSID() (string, error)                        { return "", ErrUnsupported }
