package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORSMiddlewareAllowsSameOriginAndRejectsForeignOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(corsMiddleware(nil, true))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	same := httptest.NewRequest(http.MethodGet, "http://app.example/test", nil)
	same.Host = "app.example"
	same.Header.Set("Origin", "http://app.example")
	sameW := httptest.NewRecorder()
	router.ServeHTTP(sameW, same)
	if sameW.Code != http.StatusNoContent || sameW.Header().Get("Access-Control-Allow-Origin") != "http://app.example" {
		t.Fatalf("same-origin response = %d, allow-origin=%q", sameW.Code, sameW.Header().Get("Access-Control-Allow-Origin"))
	}

	foreign := httptest.NewRequest(http.MethodGet, "http://app.example/test", nil)
	foreign.Host = "app.example"
	foreign.Header.Set("Origin", "https://evil.example")
	foreignW := httptest.NewRecorder()
	router.ServeHTTP(foreignW, foreign)
	if foreignW.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin response = %d, want %d", foreignW.Code, http.StatusForbidden)
	}
}

func TestCORSMiddlewareAllowsBrowserExtensionWithoutConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(corsMiddleware(nil, true))
	router.POST("/api/auth/login", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodOptions, "https://server.example/api/auth/login", nil)
	req.Host = "server.example"
	req.Header.Set("Origin", "chrome-extension://abcdefghijklmnopabcdefghijklmnop")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight response = %d, want %d", w.Code, http.StatusNoContent)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "chrome-extension://abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("allow-origin = %q", got)
	}
}

func TestCORSMiddlewareRejectsInvalidExtensionOrigins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, origin := range []string{
		"chrome-extension://too-short",
		"chrome-extension://qrstuvwxyzabcdefqrstuvwxyzabcdef",
		"chrome-extension://abcdefghijklmnopabcdefghijklmnop:8443",
		"moz-extension://abcdefghijklmnopabcdefghijklmnop",
	} {
		t.Run(origin, func(t *testing.T) {
			router := gin.New()
			router.Use(corsMiddleware(nil, true))
			router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			req := httptest.NewRequest(http.MethodGet, "https://server.example/test", nil)
			req.Host = "server.example"
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("response = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}

func TestCORSMiddlewareRejectsUnsafeConfiguredEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, configured := range []string{
		"*",
		"null",
		"https://trusted.example/extra",
		"https://trusted.example?trusted=true",
	} {
		t.Run(configured, func(t *testing.T) {
			router := gin.New()
			router.Use(corsMiddleware([]string{configured}, true))
			router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			req := httptest.NewRequest(http.MethodGet, "https://server.example/test", nil)
			req.Host = "server.example"
			req.Header.Set("Origin", "https://trusted.example")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("response = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}

func TestDesktopCORSMiddlewareAllowsOnlyThePinnedBrowserExtension(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(corsMiddleware([]string{desktopBrowserExtensionOrigin}, false))
	router.POST("/api/auth/login", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, test := range []struct {
		origin string
		want   int
	}{
		{origin: desktopBrowserExtensionOrigin, want: http.StatusNoContent},
		{origin: "chrome-extension://abcdefghijklmnopabcdefghijklmnop", want: http.StatusForbidden},
		{origin: "https://remote.example", want: http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/api/auth/login", nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != test.want {
			t.Fatalf("desktop CORS origin %q status = %d, want %d", test.origin, response.Code, test.want)
		}
	}
}
