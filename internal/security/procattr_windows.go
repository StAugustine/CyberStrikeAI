//go:build windows

package security

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNoWindow uint32 = 0x08000000

func hideWindowsProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

func prepareShellCmdSession(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	// 隐藏 GUI 父进程启动的控制台窗口，并保留独立进程组以便 taskkill /T 终止整棵子进程树。
	hideWindowsProcess(cmd)
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	return nil
}

// terminateProcessGroup 使用 taskkill /F /T 终止进程及其子进程；rootPID 为 0 时回退到 cmd.Process.Pid。
func terminateProcessGroup(rootPID int, cmd *exec.Cmd) {
	pid := rootPID
	if pid <= 0 && cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if pid <= 0 {
		return
	}
	tk := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	hideWindowsProcess(tk)
	if err := tk.Run(); err != nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

// terminateCmdTree 使用 taskkill /F /T 终止进程及其子进程（Windows 上 Process.Kill 无法保证杀掉 python 等孙进程）。
func terminateCmdTree(cmd *exec.Cmd) {
	terminateProcessGroup(0, cmd)
}
