package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

type installActions struct {
	service func(platform.ServiceSpec) error
	desktop func(context.Context, platform.DesktopSpec) error
}

// Installation registers only: no process start, cutover, or legacy mutation.
// Hub and desktop releases remain acceptance-gated. A collector is a bounded
// byte-forwarder, so installing it requires only the staged executable and the
// validated collector configuration; Windows still enforces protected paths.
func installVerifiedRelease(ctx context.Context, config platform.Config, configPath, executable, manifest string, actions installActions) error {
	if executable == "" {
		return errors.New("installation requires --executable")
	}
	if config.Role != platform.Hub && config.Role != platform.Collector && config.Role != platform.Desktop {
		return errors.New("unsupported installation role")
	}
	if config.Role != platform.Collector {
		if manifest == "" {
			return errors.New("hub and desktop installation requires --gate-manifest")
		}
		if _, err := platform.VerifyReleaseGate(manifest, executable); err != nil {
			return fmt.Errorf("installation acceptance evidence: %w", err)
		}
	}
	switch config.Role {
	case platform.Hub:
		return actions.service(platform.ServiceSpec{Config: config, ConfigPath: configPath, Executable: executable, GateManifest: manifest})
	case platform.Collector:
		return actions.service(platform.ServiceSpec{Config: config, ConfigPath: configPath, Executable: executable})
	case platform.Desktop:
		return actions.desktop(ctx, platform.DesktopSpec{Executable: executable, ConfigPath: configPath, OwnerSID: config.OwnerSID})
	}
	return errors.New("unsupported installation role")
}
