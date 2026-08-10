package pythonruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ExecutableEnvironment = "CYBERSTRIKE_PYTHON_EXECUTABLE"
	DirectoryEnvironment  = "CYBERSTRIKE_PYTHON_RUNTIME_DIR"
)

type manifest struct {
	SchemaVersion      int        `json:"schema_version"`
	Target             string     `json:"target"`
	PythonVersion      string     `json:"python_version"`
	PythonExecutable   string     `json:"python_executable"`
	Source             sourceInfo `json:"source"`
	DependencyLock     lockInfo   `json:"dependency_lock"`
	ThirdPartyLicenses string     `json:"third_party_licenses"`
	RequiredImports    []string   `json:"required_imports"`
}

type sourceInfo struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type lockInfo struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Configure validates a packaged Python runtime and makes it available to all
// child-process launchers in the desktop core. Empty values preserve the
// server and non-Windows desktop behavior.
func Configure(runtimeDir, executable string) error {
	runtimeDir = strings.TrimSpace(runtimeDir)
	executable = strings.TrimSpace(executable)
	if runtimeDir == "" && executable == "" {
		return nil
	}
	if runtimeDir == "" || executable == "" {
		return errors.New("desktop Python runtime directory and executable must be provided together")
	}
	runtimeDir = filepath.Clean(runtimeDir)
	executable = filepath.Clean(executable)
	if !filepath.IsAbs(runtimeDir) || !filepath.IsAbs(executable) {
		return errors.New("desktop Python runtime paths must be absolute")
	}
	expectedExecutable := filepath.Join(runtimeDir, "python.exe")
	if !samePath(executable, expectedExecutable) {
		return fmt.Errorf("desktop Python executable must be %q", expectedExecutable)
	}
	if err := requireRegularFile(executable, "executable"); err != nil {
		return err
	}

	manifestPath := filepath.Join(runtimeDir, "runtime-manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read desktop Python runtime manifest: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	var metadata manifest
	if err := decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("decode desktop Python runtime manifest: %w", err)
	}
	if metadata.SchemaVersion != 1 || metadata.Target != "x86_64-pc-windows-msvc" {
		return errors.New("desktop Python runtime manifest is incompatible")
	}
	parts := strings.Split(metadata.PythonVersion, ".")
	if len(parts) != 3 || parts[0] != "3" || parts[1] != "12" {
		return fmt.Errorf("unsupported desktop Python version %q", metadata.PythonVersion)
	}
	if metadata.PythonExecutable != "python.exe" {
		return errors.New("desktop Python runtime manifest has an invalid executable")
	}
	if !strings.HasPrefix(metadata.Source.URL, "https://www.python.org/") ||
		!validSHA256(metadata.Source.SHA256) || len(metadata.RequiredImports) == 0 {
		return errors.New("desktop Python runtime manifest has invalid source metadata")
	}
	if metadata.ThirdPartyLicenses != "THIRD-PARTY-LICENSES.json" {
		return errors.New("desktop Python runtime manifest has an invalid license inventory")
	}
	for _, required := range []string{
		"python312.dll",
		"python312.zip",
		metadata.DependencyLock.File,
		metadata.ThirdPartyLicenses,
	} {
		if required == "" || filepath.Base(required) != required {
			return errors.New("desktop Python runtime manifest has an invalid dependency lock")
		}
		if err := requireRegularFile(filepath.Join(runtimeDir, required), required); err != nil {
			return err
		}
	}
	if !validSHA256(metadata.DependencyLock.SHA256) {
		return errors.New("desktop Python runtime manifest has an invalid dependency lock hash")
	}
	lockData, err := os.ReadFile(filepath.Join(runtimeDir, metadata.DependencyLock.File))
	if err != nil {
		return fmt.Errorf("read desktop Python dependency lock: %w", err)
	}
	actualLockHash := sha256.Sum256(lockData)
	if hex.EncodeToString(actualLockHash[:]) != metadata.DependencyLock.SHA256 {
		return errors.New("desktop Python dependency lock checksum does not match the runtime manifest")
	}
	sitePackages := filepath.Join(runtimeDir, "Lib", "site-packages")
	info, err := os.Stat(sitePackages)
	if err != nil {
		return fmt.Errorf("inspect desktop Python site-packages: %w", err)
	}
	if !info.IsDir() {
		return errors.New("desktop Python site-packages is not a directory")
	}

	if err := os.Setenv(ExecutableEnvironment, executable); err != nil {
		return fmt.Errorf("set desktop Python executable: %w", err)
	}
	if err := os.Setenv(DirectoryEnvironment, runtimeDir); err != nil {
		return fmt.Errorf("set desktop Python runtime directory: %w", err)
	}
	if err := os.Setenv("PYTHONNOUSERSITE", "1"); err != nil {
		return fmt.Errorf("isolate desktop Python user packages: %w", err)
	}
	if err := os.Setenv("PYTHONDONTWRITEBYTECODE", "1"); err != nil {
		return fmt.Errorf("disable desktop Python bytecode writes: %w", err)
	}
	if err := os.Setenv("PYTHONUTF8", "1"); err != nil {
		return fmt.Errorf("enable desktop Python UTF-8 mode: %w", err)
	}
	return nil
}

// ResolveCommand maps only bare Python aliases to the validated packaged
// interpreter. Explicit paths and unrelated commands are preserved.
func ResolveCommand(command string) string {
	if strings.ContainsAny(command, `/\`) {
		return command
	}
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "python", "python.exe", "python3", "python3.exe":
		if executable := strings.TrimSpace(os.Getenv(ExecutableEnvironment)); executable != "" {
			return executable
		}
	}
	return command
}

func requireRegularFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect desktop Python %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("desktop Python %s is not a regular file", label)
	}
	return nil
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
