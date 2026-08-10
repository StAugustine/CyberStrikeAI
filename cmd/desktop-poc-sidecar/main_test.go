package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopprotocol"

	"github.com/gorilla/websocket"
)

func TestProtocolAndGracefulShutdown(t *testing.T) {
	baseURL, shutdown, done := startTestSidecar(t)
	defer shutdown()

	response, err := http.Get(baseURL + "api/poc/ping")
	if err != nil {
		t.Fatalf("GET ping: %v", err)
	}
	defer response.Body.Close()
	var ping struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&ping); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if ping.Message != "pong" {
		t.Fatalf("unexpected ping response: %#v", ping)
	}

	sseResponse, err := http.Get(baseURL + "api/poc/sse")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	sseLine, err := firstLineWithPrefix(sseResponse.Body, "data: ")
	sseResponse.Body.Close()
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	if sseLine != `data: {"sequence":1}` {
		t.Fatalf("unexpected SSE data: %q", sseLine)
	}

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "api/poc/ws"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	if err := connection.WriteMessage(websocket.TextMessage, []byte("desktop-poc")); err != nil {
		t.Fatalf("write WebSocket: %v", err)
	}
	_, payload, err := connection.ReadMessage()
	connection.Close()
	if err != nil {
		t.Fatalf("read WebSocket: %v", err)
	}
	if string(payload) != "desktop-poc" {
		t.Fatalf("unexpected WebSocket echo: %q", payload)
	}

	shutdown()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sidecar shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar did not shut down within two seconds")
	}
}

func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	baseURL, shutdown, done := startTestSidecar(t)
	defer func() {
		shutdown()
		<-done
	}()

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "api/poc/ws"
	header := http.Header{"Origin": []string{"http://example.invalid"}}
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if connection != nil {
		connection.Close()
	}
	if err == nil {
		t.Fatal("foreign WebSocket origin was accepted")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected rejection response: %#v", response)
	}
}

func startTestSidecar(t *testing.T) (string, func(), <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdinReader, stdinWriter := io.Pipe()
	go cancelOnShutdownCommand(stdinReader, cancel)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, writer)
		_ = writer.Close()
		_ = stdinReader.Close()
	}()
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			_, _ = io.WriteString(stdinWriter, "SHUTDOWN\n")
			_ = stdinWriter.Close()
		})
	}

	var ready desktopprotocol.Handshake
	if err := json.NewDecoder(reader).Decode(&ready); err != nil {
		shutdown()
		t.Fatalf("decode READY: %v", err)
	}
	_ = reader.Close()
	if ready.Type != desktopprotocol.MessageReady || ready.ProtocolVersion != desktopprotocol.Version || ready.AppVersion != pocAppVersion {
		shutdown()
		t.Fatalf("unexpected READY payload: %#v", ready)
	}
	parsed, err := url.Parse(ready.URL)
	if err != nil {
		shutdown()
		t.Fatalf("parse READY URL: %v", err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		shutdown()
		t.Fatalf("READY URL is not a random IPv4 loopback URL: %s", ready.URL)
	}
	return ready.URL, shutdown, done
}

func firstLineWithPrefix(reader io.Reader, prefix string) (string, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), prefix) {
			return scanner.Text(), nil
		}
	}
	return "", scanner.Err()
}
