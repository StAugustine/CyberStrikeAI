package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopplugin"
)

func TestRunNativeHostReturnsOnlyValidatedDiscovery(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := writeNativeHostDiscovery(t, desktopplugin.Discovery{
		SchemaVersion: desktopplugin.DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(90 * time.Second).Unix(),
	})
	input := encodeNativeHostRequest(t, nativeRequest{Operation: "discover"})
	var output bytes.Buffer
	if err := runNativeHost(bytes.NewReader(input), &output, path, now); err != nil {
		t.Fatalf("runNativeHost: %v", err)
	}
	response := decodeNativeHostResponse(t, output.Bytes())
	if !response.OK || response.Discovery == nil || response.Discovery.BaseURL != "http://127.0.0.1:43123" || response.Error != "" {
		t.Fatalf("native response = %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "token", "credential", "session"} {
		if bytes.Contains(bytes.ToLower(encoded), []byte(forbidden)) {
			t.Fatalf("native response contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestRunNativeHostFailsClosedForMissingExpiredAndUnknownRequests(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expired := writeNativeHostDiscovery(t, desktopplugin.Discovery{
		SchemaVersion: desktopplugin.DiscoverySchemaVersion,
		InstanceID:    "desktop-instance-123456",
		BaseURL:       "http://127.0.0.1:43123",
		AppVersion:    "0.1.0",
		IssuedAtUnix:  now.Add(-2 * time.Minute).Unix(),
		ExpiresAtUnix: now.Add(-time.Minute).Unix(),
	})
	for _, test := range []struct {
		name string
		path string
		body []byte
	}{
		{name: "missing", path: filepath.Join(nativeHostTestDirectory(t), desktopplugin.DiscoveryFileName), body: encodeNativeHostRequest(t, nativeRequest{Operation: "discover"})},
		{name: "expired", path: expired, body: encodeNativeHostRequest(t, nativeRequest{Operation: "discover"})},
		{name: "unknown", path: expired, body: encodeNativeHostRequest(t, nativeRequest{Operation: "export-token"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runNativeHost(bytes.NewReader(test.body), &output, test.path, now)
			if test.name == "unknown" {
				if err == nil || output.Len() != 0 {
					t.Fatalf("unknown operation err=%v output=%x", err, output.Bytes())
				}
				return
			}
			if err != nil {
				t.Fatalf("runNativeHost: %v", err)
			}
			response := decodeNativeHostResponse(t, output.Bytes())
			if response.OK || response.Discovery != nil || response.Error != "desktop integration is unavailable" {
				t.Fatalf("failure response = %#v", response)
			}
		})
	}
}

func encodeNativeHostRequest(t *testing.T, request nativeRequest) []byte {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	if err := binary.Write(&framed, binary.LittleEndian, uint32(len(payload))); err != nil {
		t.Fatal(err)
	}
	framed.Write(payload)
	return framed.Bytes()
}

func decodeNativeHostResponse(t *testing.T, framed []byte) nativeResponse {
	t.Helper()
	payload, err := readNativeMessage(bytes.NewReader(framed))
	if err != nil {
		t.Fatal(err)
	}
	var response nativeResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func writeNativeHostDiscovery(t *testing.T, discovery desktopplugin.Discovery) string {
	t.Helper()
	data, err := json.Marshal(discovery)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nativeHostTestDirectory(t), desktopplugin.DiscoveryFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func nativeHostTestDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(root, "desktop-native-host-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
