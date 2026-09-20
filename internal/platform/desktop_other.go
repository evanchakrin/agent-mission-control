//go:build !windows

package platform

import "context"

func ValidateDesktopIdentity(string) error { return ErrUnsupported }

func InstallDesktopTask(context.Context, DesktopSpec) error     { return ErrUnsupported }
func UninstallDesktopTask(context.Context, string) error        { return ErrUnsupported }
func StartDesktopTask(context.Context, string) error            { return ErrUnsupported }
func StopDesktopTask(context.Context, string) error             { return ErrUnsupported }
func DesktopTaskStatus(context.Context, string) (Status, error) { return Status{}, ErrUnsupported }
