package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestOpenAPISpecDesktopScopeExcludesOnlyUnregisteredModules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name        string
		desktopMode bool
		wantRemoved bool
	}{
		{name: "desktop", desktopMode: true, wantRemoved: true},
		{name: "server", desktopMode: false, wantRemoved: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewOpenAPIHandler(nil, zap.NewNop(), nil, nil)
			handler.SetDesktopMode(test.desktopMode)
			router := gin.New()
			router.GET("/api/openapi/spec", handler.GetOpenAPISpec)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/openapi/spec", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("OpenAPI status = %d", response.Code)
			}
			var spec map[string]interface{}
			if err := json.Unmarshal(response.Body.Bytes(), &spec); err != nil {
				t.Fatalf("decode OpenAPI: %v", err)
			}
			paths, _ := spec["paths"].(map[string]interface{})
			if _, exists := paths["/api/conversations"]; !exists {
				t.Fatal("included conversation API is missing")
			}
			for _, route := range []string{
				"/api/robot/test",
				"/api/terminal/run",
				"/api/webshell/connections",
			} {
				_, exists := paths[route]
				if exists == test.wantRemoved {
					t.Fatalf("OpenAPI route %q exists=%t, desktop=%t", route, exists, test.desktopMode)
				}
			}
		})
	}
}
