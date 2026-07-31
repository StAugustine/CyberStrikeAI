package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cyberstrike-ai/web"

	"github.com/gin-gonic/gin"
)

func TestCuratedEmbeddedWebAssetsServeWithoutWorkingDirectory(t *testing.T) {
	assets, err := prepareWebAssets(webassets.FS())
	if err != nil {
		t.Fatalf("prepare embedded web assets: %v", err)
	}

	router := gin.New()
	router.SetHTMLTemplate(assets.templates)
	router.StaticFS("/static", assets.static)
	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{"Version": "v-test"})
	})

	root := httptest.NewRecorder()
	router.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "v-test") {
		t.Fatalf("embedded index response status=%d contains_version=%t", root.Code, strings.Contains(root.Body.String(), "v-test"))
	}

	coreAsset := httptest.NewRecorder()
	router.ServeHTTP(coreAsset, httptest.NewRequest(http.MethodGet, "/static/js/chat.js", nil))
	if coreAsset.Code != http.StatusOK {
		t.Fatalf("embedded core asset status = %d, want 200", coreAsset.Code)
	}

	excludedAsset := httptest.NewRecorder()
	router.ServeHTTP(excludedAsset, httptest.NewRequest(http.MethodGet, "/static/js/c2.js", nil))
	if excludedAsset.Code != http.StatusNotFound {
		t.Fatalf("excluded desktop asset status = %d, want 404", excludedAsset.Code)
	}
}
