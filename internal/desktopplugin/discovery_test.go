package desktopplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDiscoveryAcceptsShortLivedLoopbackMetadata(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := writeDiscoveryTestFile(t, Discovery{
		SchemaVersion: DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(90 * time.Second).Unix(),
	})
	discovery, err := LoadDiscovery(path, now)
	if err != nil || discovery.BaseURL != "http://127.0.0.1:43123" {
		t.Fatalf("LoadDiscovery = %#v, %v", discovery, err)
	}
}

func TestLoadDiscoveryRejectsExpiredRemoteAndSecretBearingDocuments(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := Discovery{
		SchemaVersion: DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(90 * time.Second).Unix(),
	}
	tests := []struct {
		name   string
		mutate func(*Discovery)
		want   string
	}{
		{name: "expired", mutate: func(value *Discovery) {
			value.IssuedAtUnix = now.Add(-2 * time.Minute).Unix()
			value.ExpiresAtUnix = now.Add(-time.Minute).Unix()
		}, want: "expired"},
		{name: "remote", mutate: func(value *Discovery) { value.BaseURL = "http://192.0.2.1:43123" }, want: "endpoint"},
		{name: "long lifetime", mutate: func(value *Discovery) { value.ExpiresAtUnix = now.Add(3 * time.Minute).Unix() }, want: "lifetime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := valid
			test.mutate(&value)
			_, err := LoadDiscovery(writeDiscoveryTestFile(t, value), now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadDiscovery error = %v, want %q", err, test.want)
			}
		})
	}

	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"token":"must-not-be-accepted"}`)...)
	path := filepath.Join(discoveryTestDirectory(t), DiscoveryFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDiscovery(path, now); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("secret-bearing discovery error = %v", err)
	}
}

func TestLoadDiscoveryRejectsBroadPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows discovery privacy is enforced by the inherited user ACL")
	}
	now := time.Unix(1_800_000_000, 0)
	path := writeDiscoveryTestFile(t, Discovery{
		SchemaVersion: DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(time.Minute).Unix(),
	})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDiscovery(path, now); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("LoadDiscovery permissions error = %v", err)
	}
}

func TestLoadDiscoveryRejectsSymbolicLink(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	target := writeDiscoveryTestFile(t, Discovery{
		SchemaVersion: DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(time.Minute).Unix(),
	})
	link := filepath.Join(discoveryTestDirectory(t), DiscoveryFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := LoadDiscovery(link, now); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadDiscovery symlink error = %v", err)
	}
}

func writeDiscoveryTestFile(t *testing.T, discovery Discovery) string {
	t.Helper()
	data, err := json.Marshal(discovery)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(discoveryTestDirectory(t), DiscoveryFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func discoveryTestDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(root, "desktop-plugin-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
