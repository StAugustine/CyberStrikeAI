package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func TestHealthRoutesReflectServeReadiness(t *testing.T) {
	application := newServerTestApp()
	registerHealthRoutes(application.router, application)

	assertHealthResponse(t, application.router, "/health/live", http.StatusOK, "live", "v-test")
	assertHealthResponse(t, application.router, "/health/ready", http.StatusServiceUnavailable, "not_ready", "v-test")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- application.Serve(ctx, listener)
	}()

	readyURL := "http://" + listener.Addr().String() + "/health/ready"
	waitForReady(t, readyURL)
	if !application.Ready() {
		t.Fatal("application should report ready while serving")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve returned error during graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after context cancellation")
	}
	if application.Ready() {
		t.Fatal("application should not report ready after shutdown")
	}
}

func TestServeClosesListenerWhenContextAlreadyCancelled(t *testing.T) {
	application := newServerTestApp()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := application.Serve(ctx, listener); err != nil {
		t.Fatalf("serve with cancelled context: %v", err)
	}
	if err := listener.Close(); err == nil {
		t.Fatal("listener should already be closed")
	}
}

func TestServeCancellationStopsSSEAndWebSocketConnections(t *testing.T) {
	application := newServerTestApp()
	sseStopped := make(chan struct{})
	application.router.GET("/test/sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = io.WriteString(c.Writer, "data: ready\n\n")
		c.Writer.Flush()
		<-c.Request.Context().Done()
		close(sseStopped)
	})
	webSocketStopped := make(chan struct{})
	application.router.GET("/test/ws", func(c *gin.Context) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer close(webSocketStopped)
		defer connection.Close()
		_, _, _ = connection.ReadMessage()
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- application.Serve(ctx, listener)
	}()

	baseURL := "http://" + listener.Addr().String()
	sseResponse, err := http.Get(baseURL + "/test/sse")
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer sseResponse.Body.Close()
	data := make([]byte, len("data: ready\n\n"))
	if _, err := io.ReadFull(sseResponse.Body, data); err != nil {
		t.Fatalf("read SSE event: %v", err)
	}

	webSocketURL := "ws" + baseURL[len("http"):] + "/test/ws"
	connection, _, err := websocket.DefaultDialer.Dial(webSocketURL, nil)
	if err != nil {
		t.Fatalf("open WebSocket: %v", err)
	}
	defer connection.Close()

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve returned error during protocol shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop with active SSE and WebSocket connections")
	}
	for name, stopped := range map[string]<-chan struct{}{"SSE": sseStopped, "WebSocket": webSocketStopped} {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatalf("%s handler did not stop", name)
		}
	}
}

func TestShutdownIsIdempotentAndStopsRobotConnections(t *testing.T) {
	application := newServerTestApp()
	var cancellations atomic.Int32
	newCancel := func() context.CancelFunc {
		return func() { cancellations.Add(1) }
	}
	application.dingCancel = newCancel()
	application.larkCancel = newCancel()
	application.wechatCancel = newCancel()
	application.telegramCancel = newCancel()
	application.slackCancel = newCancel()
	application.discordCancel = newCancel()
	application.qqCancel = newCancel()

	application.Shutdown()
	application.Shutdown()

	if got := cancellations.Load(); got != 7 {
		t.Fatalf("robot cancellations = %d, want 7", got)
	}
}

func newServerTestApp() *App {
	gin.SetMode(gin.TestMode)
	return &App{
		config: &config.Config{
			Version: "v-test",
			Server: config.ServerConfig{
				Host: "127.0.0.1",
			},
		},
		logger: &logger.Logger{Logger: zap.NewNop()},
		router: gin.New(),
	}
}

func assertHealthResponse(t *testing.T, handler http.Handler, path string, wantCode int, wantStatus, wantVersion string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != wantCode {
		t.Fatalf("GET %s status = %d, want %d", path, recorder.Code, wantCode)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET %s response: %v", path, err)
	}
	if body.Status != wantStatus || body.Version != wantVersion {
		t.Fatalf("GET %s body = %+v, want status %q version %q", path, body, wantStatus, wantVersion)
	}
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", url)
}
