package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"cyberstrike-ai/internal/config"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

type appServeState struct {
	ready atomic.Bool
}

// Ready reports whether the main HTTP server has entered its serve loop.
func (a *App) Ready() bool {
	return a != nil && a.serveState.ready.Load()
}

// Run starts the application using the configured address.
func (a *App) Run() error {
	return a.RunWithContext(context.Background())
}

// RunWithContext starts the application using the configured address and
// gracefully stops its HTTP servers when ctx is cancelled.
func (a *App) RunWithContext(ctx context.Context) error {
	if a == nil || a.config == nil {
		return errors.New("application config is required")
	}
	addr := fmt.Sprintf("%s:%d", a.config.Server.Host, a.config.Server.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return a.Serve(ctx, listener)
}

// Serve runs the main HTTP server on a caller-provided listener. The listener
// is owned by Serve after this call and is closed during shutdown.
func (a *App) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("main server listener is required")
	}
	if a == nil || a.config == nil || a.router == nil || a.logger == nil {
		_ = listener.Close()
		return errors.New("application is not initialized")
	}
	if ctx == nil {
		_ = listener.Close()
		return errors.New("serve context is required")
	}
	if ctx.Err() != nil {
		_ = listener.Close()
		return nil
	}

	tlsMode, tlsConf, certFile, keyFile, err := prepareMainServerTLS(&a.config.Server)
	if err != nil {
		_ = listener.Close()
		return err
	}

	addr := listener.Addr().String()
	srv := &http.Server{Addr: addr, Handler: a.router}
	serveListener := listener
	var mainMux *mainServerMux
	httpRedirect := config.ServerHTTPRedirectEnabled(&a.config.Server)
	if tlsMode != mainTLSOff {
		tlsConf, err = ensureMainTLSConfigCerts(tlsMode, tlsConf, certFile, keyFile)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("加载 TLS 证书: %w", err)
		}
		srv.TLSConfig = tlsConf
		if err := http2.ConfigureServer(srv, &http2.Server{}); err != nil {
			_ = listener.Close()
			return fmt.Errorf("主服务 HTTP/2 配置失败: %w", err)
		}
		if httpRedirect {
			mainMux = newMainServerMux(listener, srv, portFromListenAddr(addr), a.logger.Logger)
		} else {
			serveListener = tls.NewListener(listener, tlsConf)
		}
		switch tlsMode {
		case mainTLSFromFiles:
			a.logger.Debug("启动 HTTPS 主服务（已启用 HTTP/2 协商）", zap.String("address", addr), zap.String("cert", certFile))
		case mainTLSInMemorySelfSigned:
			a.logger.Debug("启动 HTTPS 主服务（内存自签证书，仅测试；已启用 HTTP/2 协商）", zap.String("address", addr))
		}
		if httpRedirect {
			a.logger.Debug("已启用 HTTP→HTTPS 自动跳转（同端口嗅探分流）", zap.String("address", addr))
		}
	} else {
		a.logger.Debug("启动 HTTP 主服务", zap.String("address", addr))
	}

	var mcpServer *http.Server
	if a.config.MCP.Enabled {
		mcpAddr := fmt.Sprintf("%s:%d", a.config.MCP.Host, a.config.MCP.Port)
		a.logger.Info("启动MCP服务器", zap.String("address", mcpAddr))
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", a.mcpHandlerWithAuth)
		mcpServer = &http.Server{Addr: mcpAddr, Handler: mux}
		go func() {
			if serveErr := mcpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				a.logger.Error("MCP服务器启动失败", zap.Error(serveErr))
			}
		}()
	}

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if mainMux != nil {
				if shutdownErr := mainMux.Shutdown(shutdownCtx); shutdownErr != nil {
					a.logger.Error("HTTP/HTTPS 分流服务器关闭失败", zap.Error(shutdownErr))
				}
			} else if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
				a.logger.Error("HTTP服务器关闭失败", zap.Error(shutdownErr))
			}
			if mcpServer != nil {
				if shutdownErr := mcpServer.Shutdown(shutdownCtx); shutdownErr != nil {
					a.logger.Error("MCP服务器关闭失败", zap.Error(shutdownErr))
				}
			}
		})
	}

	serveStopped := make(chan struct{})
	shutdownFinished := make(chan struct{})
	go func() {
		defer close(shutdownFinished)
		select {
		case <-ctx.Done():
			shutdown()
		case <-serveStopped:
		}
	}()

	a.serveState.ready.Store(true)
	if mainMux != nil {
		err = mainMux.Serve()
	} else {
		err = srv.Serve(serveListener)
	}
	a.serveState.ready.Store(false)
	close(serveStopped)
	shutdown()
	<-shutdownFinished

	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func registerHealthRoutes(router *gin.Engine, app *App) {
	router.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live", "version": appVersion(app)})
	})
	router.GET("/health/ready", func(c *gin.Context) {
		if !app.Ready() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "version": appVersion(app)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "version": appVersion(app)})
	})
}

func appVersion(app *App) string {
	if app != nil && app.config != nil && app.config.Version != "" {
		return app.config.Version
	}
	return "unknown"
}
