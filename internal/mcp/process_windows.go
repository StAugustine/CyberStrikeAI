//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

const createNoWindow uint32 = 0x08000000

func prepareExternalCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}
