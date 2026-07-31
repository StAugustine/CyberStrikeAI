package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopprotocol"
	"cyberstrike-ai/internal/desktopruntime"
	"github.com/gin-gonic/gin"
)

func TestDesktopCoreBootstrapReadyAndShutdown(t *testing.T) {
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
