package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopcredentials"
	"cyberstrike-ai/internal/desktopprotocol"
	"cyberstrike-ai/internal/desktopruntime"
	"github.com/gin-gonic/gin"
)

func TestDesktopCoreLocalAdminGoldenPath(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "test-version")
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir: resourceDir,
		AppVersion:  "test-version",
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create desktop stdout pipe: %v", err)
	}
	previousGinWriter := gin.DefaultWriter
	gin.DefaultWriter = stdoutWriter
	done := make(chan error, 1)
	go func() {
		done <- runDesktopCore(context.Background(), stdinReader, stdoutWriter, options)
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
	}()
	t.Cleanup(func() {
		gin.DefaultWriter = previousGinWriter
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})

	var stdoutTranscript bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(stdoutReader, &stdoutTranscript))
	var bootstrap desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &bootstrap)
	if bootstrap.Type != desktopprotocol.MessageBootstrapRequired || bootstrap.AppVersion != options.AppVersion {
		t.Fatalf("unexpected bootstrap handshake: %#v", bootstrap)
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandBootstrap,
		ProtocolVersion: desktopprotocol.Version,
		Password:        "desktop-secret",
	}); err != nil {
		t.Fatalf("write bootstrap command: %v", err)
	}

	var ready desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &ready)
	if err := ready.Validate(); err != nil {
		t.Fatalf("READY validation: %v", err)
	}
	if ready.Type != desktopprotocol.MessageReady || ready.AppVersion != options.AppVersion {
		t.Fatalf("unexpected READY handshake: %#v", ready)
	}
	response, err := http.Get(ready.URL + "health/ready")
	if err != nil {
		t.Fatalf("GET health ready: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health ready status = %d", response.StatusCode)
	}

	status, _ := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/conversations", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated conversations status = %d", status)
	}
	status, _ = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "wrong-password",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-password login status = %d", status)
	}
	status, login := desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "desktop-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("local admin login status = %d, body = %#v", status, login)
	}
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("local admin login did not return a token: %#v", login)
	}
	user, _ := login["user"].(map[string]interface{})
	if user["username"] != "admin" {
		t.Fatalf("local admin login returned unexpected user: %#v", login)
	}
	permissions, _ := login["permissions"].([]interface{})
	if len(permissions) == 0 {
		t.Fatalf("local admin login returned no permissions: %#v", login)
	}

	for _, path := range []string{
		"api/auth/validate",
		"api/conversations",
		"api/monitor/stats",
		"api/notifications/summary",
		"api/config",
	} {
		status, body := desktopJSONRequest(t, http.MethodGet, ready.URL+path, token, nil)
		if status != http.StatusOK {
			t.Fatalf("authenticated GET /%s status = %d, body = %#v", path, status, body)
		}
	}

	status, body := desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/logout", token, nil)
	if status != http.StatusOK {
		t.Fatalf("local admin logout status = %d, body = %#v", status, body)
	}
	status, _ = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/auth/validate", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token validation status = %d", status)
	}

	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandShutdown,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write shutdown command: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDesktopCore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("desktop core did not shut down")
	}
	if _, err := io.Copy(&stdoutTranscript, stdoutReader); err != nil {
		t.Fatalf("drain desktop stdout: %v", err)
	}
	protocolLines := strings.Split(strings.TrimSpace(stdoutTranscript.String()), "\n")
	if len(protocolLines) != 2 {
		t.Fatalf("desktop stdout must contain exactly two protocol messages, got %d: %q", len(protocolLines), stdoutTranscript.String())
	}
	for index, line := range protocolLines {
		var handshake desktopprotocol.Handshake
		if err := json.Unmarshal([]byte(line), &handshake); err != nil {
			t.Fatalf("desktop stdout message %d is not valid JSON: %v", index, err)
		}
	}
	if strings.Contains(stdoutTranscript.String(), "desktop-secret") {
		t.Fatal("bootstrap password leaked to desktop stdout")
	}
	assertSecretNotPersisted(t, root, "desktop-secret")
}

func desktopJSONRequest(t *testing.T, method, target, token string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode desktop request body: %v", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, requestBody)
	if err != nil {
		t.Fatalf("create desktop request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send desktop request: %v", err)
	}
	defer response.Body.Close()
	payload := make(map[string]interface{})
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("decode desktop response: %v", err)
	}
	return response.StatusCode, payload
}

func TestDesktopCoreMigratesCredentialsOnlyAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "test-version")
	appendTestResourceConfig(t, resourceDir, "test-version", "fofa:\n  api_key: migration-secret\n")
	store := newRecordingCredentialStore()
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir:     resourceDir,
		AppVersion:      "test-version",
		CredentialStore: store,
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runDesktopCore(context.Background(), stdinReader, stdoutWriter, options)
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})

	var stdoutTranscript bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(stdoutReader, &stdoutTranscript))
	var migration desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &migration)
	if err := migration.Validate(); err != nil {
		t.Fatalf("migration validation: %v", err)
	}
	if migration.Type != desktopprotocol.MessageCredentialMigrationRequired || len(migration.CredentialPaths) != 1 || migration.CredentialPaths[0] != desktopcredentials.PathFOFA {
		t.Fatalf("unexpected migration handshake: %#v", migration)
	}
	if len(store.values) != 0 {
		t.Fatalf("credential store changed before confirmation: %v", store.values)
	}
	if strings.Contains(stdoutTranscript.String(), "migration-secret") {
		t.Fatal("migration handshake exposed plaintext")
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandMigrateCredentials,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write migration command: %v", err)
	}

	var bootstrap desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &bootstrap)
	if bootstrap.Type != desktopprotocol.MessageBootstrapRequired {
		t.Fatalf("unexpected bootstrap handshake: %#v", bootstrap)
	}
	if len(store.values) != 1 {
		t.Fatalf("credential store values after confirmation = %v", store.values)
	}
	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configData, []byte("migration-secret")) || !bytes.Contains(configData, []byte(desktopcredentials.ReferencePrefix)) {
		t.Fatalf("migrated config did not contain only a credential reference: %q", configData)
	}

	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandBootstrap,
		ProtocolVersion: desktopprotocol.Version,
		Password:        "desktop-secret",
	}); err != nil {
		t.Fatalf("write bootstrap command: %v", err)
	}
	var ready desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &ready)
	if ready.Type != desktopprotocol.MessageReady {
		t.Fatalf("unexpected READY handshake: %#v", ready)
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandShutdown,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write shutdown command: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDesktopCore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("desktop core did not shut down")
	}
	if strings.Contains(stdoutTranscript.String(), "migration-secret") {
		t.Fatal("desktop stdout exposed migrated plaintext")
	}
}

func TestDesktopCoreRejectsResourceVersionMismatch(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "resource-version")
	err := runDesktopCore(context.Background(), emptyReader{}, io.Discard, runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir: resourceDir,
		AppVersion:  "different-version",
	})
	if err == nil {
		t.Fatal("expected resource version mismatch")
	}
}

func TestDesktopCoreFailsClosedWhenCredentialStoreIsUnavailable(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "test-version")
	appendTestResourceConfig(t, resourceDir, "test-version", "fofa:\n  api_key: migration-secret\n")

	stdin := bytes.NewBufferString(`{"type":"MIGRATE_CREDENTIALS","protocol_version":1}` + "\n")
	err := runDesktopCore(context.Background(), stdin, io.Discard, runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir:     resourceDir,
		AppVersion:      "test-version",
		CredentialStore: failingCredentialStore{},
	})
	if err == nil || !strings.Contains(err.Error(), "store desktop credential fofa.api_key") {
		t.Fatalf("unexpected credential store error: %v", err)
	}
	if strings.Contains(err.Error(), "migration-secret") {
		t.Fatal("credential store error exposed plaintext")
	}
}

type failingCredentialStore struct{}

func (failingCredentialStore) Get(string) (string, error) {
	return "", errors.New("credential store unavailable")
}

func (failingCredentialStore) Set(string, string) error {
	return errors.New("credential store unavailable")
}

func (failingCredentialStore) Delete(string) error { return nil }

type recordingCredentialStore struct {
	values map[string]string
}

func newRecordingCredentialStore() *recordingCredentialStore {
	return &recordingCredentialStore{values: make(map[string]string)}
}

func (s *recordingCredentialStore) Get(account string) (string, error) {
	value, ok := s.values[account]
	if !ok {
		return "", errors.New("credential not found")
	}
	return value, nil
}

func (s *recordingCredentialStore) Set(account, secret string) error {
	s.values[account] = secret
	return nil
}

func (s *recordingCredentialStore) Delete(account string) error {
	delete(s.values, account)
	return nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func writeTestResources(t *testing.T, root, version string) string {
	t.Helper()
	resourceDir := filepath.Join(root, "bundled-defaults")
	if err := os.MkdirAll(resourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`version: test
server:
  host: 127.0.0.1
  port: 0
auth:
  session_duration_hours: 12
log:
  level: error
  output: stdout
knowledge:
  enabled: false
c2:
  enabled: false
`)
	if err := os.WriteFile(filepath.Join(resourceDir, "config.example.yaml"), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(configData)
	manifest := desktopruntime.ResourceManifest{
		SchemaVersion: 1,
		AppVersion:    version,
		Files: []desktopruntime.ResourceFile{{
			Path:   "config.example.yaml",
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	return resourceDir
}

func appendTestResourceConfig(t *testing.T, resourceDir, version, extra string) {
	t.Helper()
	configPath := filepath.Join(resourceDir, "config.example.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configData = append(configData, []byte(extra)...)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(configData)
	manifestData, err := json.Marshal(desktopruntime.ResourceManifest{
		SchemaVersion: 1,
		AppVersion:    version,
		Files: []desktopruntime.ResourceFile{{
			Path:   "config.example.yaml",
			SHA256: hex.EncodeToString(sum[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeWithTimeout(t *testing.T, decoder *json.Decoder, target any) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- decoder.Decode(target) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode desktop handshake: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for desktop handshake")
	}
}

func assertSecretNotPersisted(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("bootstrap password persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan desktop files for plaintext password: %v", err)
	}
}
