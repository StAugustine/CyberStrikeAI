//go:build !windows

package mcp

import "os/exec"

func prepareExternalCommand(_ *exec.Cmd) {}
