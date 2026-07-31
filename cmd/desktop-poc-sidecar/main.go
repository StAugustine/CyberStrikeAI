package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cyberstrike-ai/internal/desktopprotocol"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const pocAppVersion = "d1-poc"

type browserProtocolResult struct {
	REST                      bool `json:"rest"`
	SSE                       bool `json:"sse"`
	WebSocket                 bool `json:"websocket"`
	ExternalNavigationBlocked bool `json:"external_navigation_blocked"`
}

type pocResultMessage struct {
	Type string `json:"type"`
	browserProtocolResult
}

func main() {
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	ctx, cancel := context.WithCancel(signalContext)
	defer cancel()
	if os.Getenv("CYBERSTRIKE_DESKTOP_POC_IGNORE_SHUTDOWN") != "1" {
		go cancelOnShutdownCommand(os.Stdin, cancel)
	}

	if err := run(ctx, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "desktop PoC sidecar failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout io.Writer) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}

	results := make(chan browserProtocolResult, 1)
	server := &http.Server{
		Handler:           newRouter(results),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	message := desktopprotocol.NewReady(pocAppVersion, "http://"+listener.Addr().String()+"/")
	encoder := json.NewEncoder(stdout)
	if err := encoder.Encode(message); err != nil {
		_ = server.Close()
		return fmt.Errorf("write READY handshake: %w", err)
	}
	if value := os.Getenv("CYBERSTRIKE_DESKTOP_POC_SIDECAR_CRASH_MS"); value != "" {
		milliseconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			_ = server.Close()
			return fmt.Errorf("parse sidecar crash delay: %w", err)
		}
		go func() {
			time.Sleep(time.Duration(milliseconds) * time.Millisecond)
			os.Exit(17)
		}()
	}

	resultEvents := (<-chan browserProtocolResult)(results)
	shutdownRequested := false
	for !shutdownRequested {
		select {
		case result := <-resultEvents:
			if err := encoder.Encode(pocResultMessage{Type: "POC_RESULT", browserProtocolResult: result}); err != nil {
				_ = server.Close()
				return fmt.Errorf("write browser protocol result: %w", err)
			}
			resultEvents = nil
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve loopback HTTP: %w", err)
			}
			return nil
		case <-ctx.Done():
			shutdownRequested = true
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown loopback HTTP: %w", err)
	}

	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve loopback HTTP: %w", err)
	}
	return nil
}

func cancelOnShutdownCommand(stdin io.Reader, cancel context.CancelFunc) {
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "SHUTDOWN" {
			cancel()
			return
		}
	}
}

func newRouter(results chan<- browserProtocolResult) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(pocPage))
	})
	router.GET("/health/ready", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ready", "protocol_version": desktopprotocol.Version})
	})
	router.GET("/api/poc/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})
	router.GET("/api/poc/sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		_, _ = io.WriteString(c.Writer, "event: message\ndata: {\"sequence\":1}\n\n")
		c.Writer.Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(c.Writer, "event: message\ndata: {\"sequence\":2}\n\n")
		c.Writer.Flush()
	})
	router.GET("/api/poc/ws", func(c *gin.Context) {
		connection, err := pocUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			return
		}
		_ = connection.WriteMessage(messageType, payload)
	})
	router.GET("/api/poc/download", func(c *gin.Context) {
		c.Header("Content-Disposition", `attachment; filename="desktop-poc.txt"`)
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("desktop-poc\n"))
	})
	router.POST("/api/poc/result", func(c *gin.Context) {
		var result browserProtocolResult
		if err := c.ShouldBindJSON(&result); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid protocol result"})
			return
		}
		select {
		case results <- result:
		default:
		}
		c.Status(http.StatusNoContent)
	})

	return router
}

var pocUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Scheme == "http" && parsed.Host == r.Host && parsed.Hostname() == "127.0.0.1"
	},
}

const pocPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>CyberStrikeAI Desktop Protocol PoC</title>
  <style>
    body { font: 16px/1.5 system-ui, sans-serif; margin: 2rem; color: #172033; }
    main { max-width: 760px; margin: 0 auto; }
    li { margin: .5rem 0; }
    .pending { color: #755f00; }
    .ok { color: #087a3e; }
    .failed { color: #b42318; }
  </style>
</head>
<body>
  <main>
    <h1>CyberStrikeAI Desktop Protocol PoC</h1>
    <p>This page is served by the Go sidecar over a random IPv4 loopback port.</p>
    <ul>
      <li id="rest" class="pending">REST: pending</li>
      <li id="sse" class="pending">SSE: pending</li>
      <li id="ws" class="pending">WebSocket: pending</li>
      <li id="external" class="pending">External navigation: pending</li>
      <li id="download" class="pending">Download interception: pending</li>
    </ul>
  </main>
  <script>
    const mark = (id, ok, detail) => {
      const node = document.getElementById(id);
      node.className = ok ? 'ok' : 'failed';
      node.textContent = id.toUpperCase() + ': ' + (ok ? 'ok' : 'failed') + (detail ? ' (' + detail + ')' : '');
    };

    const testREST = async () => {
      try {
        const response = await fetch('/api/poc/ping');
        const value = await response.json();
        const ok = value.message === 'pong';
        mark('rest', ok);
        return ok;
      } catch (error) {
        mark('rest', false, error.message);
        return false;
      }
    };

    const testSSE = () => new Promise((resolve) => {
      const events = new EventSource('/api/poc/sse');
      let settled = false;
      const finish = (ok, detail) => {
        if (settled) return;
        settled = true;
        events.close();
        mark('sse', ok, detail);
        resolve(ok);
      };
      events.onmessage = (event) => finish(JSON.parse(event.data).sequence === 1);
      events.onerror = () => finish(false, 'stream error');
    });

    const testWebSocket = () => new Promise((resolve) => {
      const socketProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const socket = new WebSocket(socketProtocol + '//' + location.host + '/api/poc/ws');
      let settled = false;
      const finish = (ok, detail) => {
        if (settled) return;
        settled = true;
        socket.close();
        mark('ws', ok, detail);
        resolve(ok);
      };
      socket.onopen = () => socket.send('desktop-poc');
      socket.onmessage = (event) => finish(event.data === 'desktop-poc');
      socket.onerror = () => finish(false, 'socket error');
    });

    const testExternalNavigation = () => new Promise((resolve) => {
      const expectedOrigin = location.origin;
      location.href = 'https://example.invalid/desktop-poc';
      window.setTimeout(() => {
        const blocked = location.origin === expectedOrigin;
        mark('external', blocked);
        resolve(blocked);
      }, 100);
    });

    const requestDownload = () => {
      const link = document.createElement('a');
      link.href = '/api/poc/download';
      link.download = 'desktop-poc.txt';
      link.click();
      mark('download', true, 'requested');
    };

    Promise.all([testREST(), testSSE(), testWebSocket()])
      .then(async ([rest, sse, websocket]) => {
        const externalNavigationBlocked = await testExternalNavigation();
        requestDownload();
        return fetch('/api/poc/result', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ rest, sse, websocket, external_navigation_blocked: externalNavigationBlocked })
        });
      });
  </script>
</body>
</html>`
