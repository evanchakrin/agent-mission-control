//go:build !windows

package desktop

import "os/exec"

func hideCommand(*exec.Cmd) {}
