package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistAuthPasswordQuotesYAMLSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := strings.Join([]string{
		"server:",
		"  host: 0.0.0.0",
		"auth:",
		"  password: old-password # Web 登录密码",
		"  session_duration_hours: 12",
		"log:",
		"  level: info",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	want := `@abc:def # still password`
	if err := PersistAuthPassword(path, want); err != nil {
		t.Fatalf("PersistAuthPassword: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), `password: "@abc:def # still password" # Web 登录密码`) {
		t.Fatalf("password was not safely quoted or comment was not preserved:\n%s", data)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after PersistAuthPassword: %v", err)
	}
	if cfg.Auth.Password != want {
		t.Fatalf("Auth.Password = %q, want %q", cfg.Auth.Password, want)
	}
}

func TestPersistAuthPasswordDoesNotTreatQuotedHashAsComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := strings.Join([]string{
		"auth:",
		`  password: "old#password"`,
		"  session_duration_hours: 12",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := PersistAuthPassword(path, "new-password"); err != nil {
		t.Fatalf("PersistAuthPassword: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "#password") {
		t.Fatalf("old quoted password fragment was incorrectly preserved as a comment:\n%s", data)
	}
}
