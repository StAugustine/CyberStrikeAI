//go:build windows

package security

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestPrepareShellCmdSessionHidesWindowAndPreservesFlags(t *testing.T) {
	const existingFlag uint32 = 0x00000004
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: existingFlag}

	if err := prepareShellCmdSession(cmd); err != nil {
		t.Fatalf("prepareShellCmdSession: %v", err)
	}

	want := existingFlag | createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP
	if got := cmd.SysProcAttr.CreationFlags; got != want {
		t.Fatalf("CreationFlags = %#x, want %#x", got, want)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}

func TestHideWindowsProcessInitializesAttributes(t *testing.T) {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", "1")

	hideWindowsProcess(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if got := cmd.SysProcAttr.CreationFlags; got != createNoWindow {
		t.Fatalf("CreationFlags = %#x, want %#x", got, createNoWindow)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}
