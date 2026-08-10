package pythonruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureAndResolvePackagedRuntime(t *testing.T) {
	runtimeDir, executable := testRuntime(t)
	t.Setenv(ExecutableEnvironment, "")
	t.Setenv(DirectoryEnvironment, "")
	t.Setenv("PYTHONNOUSERSITE", "")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "")
	t.Setenv("PYTHONUTF8", "")

	if err := Configure(runtimeDir, executable); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	for _, alias := range []string{"python", "python.exe", "python3", "python3.exe"} {
		if got := ResolveCommand(alias); got != executable {
			t.Errorf("ResolveCommand(%q) = %q, want %q", alias, got, executable)
		}
	}
	if got := ResolveCommand("/usr/bin/python3"); got != "/usr/bin/python3" {
		t.Fatalf("explicit Python path changed to %q", got)
	}
	if got := ResolveCommand("nmap"); got != "nmap" {
		t.Fatalf("unrelated command changed to %q", got)
	}
	if os.Getenv("PYTHONNOUSERSITE") != "1" ||
		os.Getenv("PYTHONDONTWRITEBYTECODE") != "1" ||
		os.Getenv("PYTHONUTF8") != "1" {
		t.Fatal("packaged Python isolation environment was not configured")
	}
}

func TestConfigureRejectsIncompleteOrTamperedRuntime(t *testing.T) {
	runtimeDir, executable := testRuntime(t)
	if err := Configure(runtimeDir, ""); err == nil {
		t.Fatal("Configure() accepted a missing executable")
	}
	if err := Configure(runtimeDir, filepath.Join(runtimeDir, "other.exe")); err == nil {
		t.Fatal("Configure() accepted a different executable")
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "DEPENDENCIES.lock"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Configure(runtimeDir, executable); err == nil {
		t.Fatal("Configure() accepted a tampered dependency lock")
	}
}

func TestConfigureAllowsAbsentRuntime(t *testing.T) {
	if err := Configure("", ""); err != nil {
		t.Fatalf("Configure() empty runtime error = %v", err)
	}
}

func testRuntime(t *testing.T) (string, string) {
	t.Helper()
	runtimeDir := t.TempDir()
	for _, file := range []string{
		"python.exe",
		"python312.dll",
		"python312.zip",
		"THIRD-PARTY-LICENSES.json",
	} {
		if err := os.WriteFile(filepath.Join(runtimeDir, file), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(runtimeDir, "Lib", "site-packages"), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := []byte("requests==2.32.0\n")
	if err := os.WriteFile(filepath.Join(runtimeDir, "DEPENDENCIES.lock"), lock, 0o600); err != nil {
		t.Fatal(err)
	}
	lockHash := sha256.Sum256(lock)
	metadata := manifest{
		SchemaVersion:    1,
		Target:           "x86_64-pc-windows-msvc",
		PythonVersion:    "3.12.10",
		PythonExecutable: "python.exe",
		Source: sourceInfo{
			URL:    "https://www.python.org/python.zip",
			SHA256: strings.Repeat("a", 64),
		},
		ThirdPartyLicenses: "THIRD-PARTY-LICENSES.json",
		DependencyLock: lockInfo{
			File:   "DEPENDENCIES.lock",
			SHA256: hex.EncodeToString(lockHash[:]),
		},
		RequiredImports: []string{"requests"},
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "runtime-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return runtimeDir, filepath.Join(runtimeDir, "python.exe")
}
