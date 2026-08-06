package handler

import (
	"os"
	"testing"
)

func TestConfiguredToolRuntimeDistinguishesDefinitionFromExecutable(t *testing.T) {
	kind, command, path, available := configuredToolRuntime("internal:query_execution_result")
	if kind != "builtin" || command != "" || path != "" || available != nil {
		t.Fatalf("builtin tool runtime = %q, %q, %q, %#v", kind, command, path, available)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	kind, command, path, available = configuredToolRuntime(executable)
	if kind != "system_command" || command != executable || path == "" || available == nil || !*available {
		t.Fatalf("available tool runtime = %q, %q, %q, %#v", kind, command, path, available)
	}

	kind, command, path, available = configuredToolRuntime("cyberstrike-desktop-command-that-does-not-exist")
	if kind != "system_command" || command == "" || path != "" || available == nil || *available {
		t.Fatalf("missing tool runtime = %q, %q, %q, %#v", kind, command, path, available)
	}
}
