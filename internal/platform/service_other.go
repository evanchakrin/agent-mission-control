//go:build !windows

package platform

import "context"

func InstallService(ServiceSpec) error         { return ErrUnsupported }
func StartService(context.Context, Role) error { return ErrUnsupported }
func StopService(context.Context, Role) error  { return ErrUnsupported }
func ServiceStatus(role Role) (Status, error) {
	return Status{Name: role.ServiceName(), State: "unsupported"}, ErrUnsupported
}
func UninstallService(context.Context, Role) error { return ErrUnsupported }
func RunService(ctx context.Context, role Role, run func(context.Context) error) error {
	return invoke(ctx, run)
}
