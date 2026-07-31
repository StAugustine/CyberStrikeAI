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
		c.HTML(http.StatusOK, "index.html", gin.H{"DesktopMode": true, "Version": "v-test"})
	})
	router.GET("/server", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{"DesktopMode": false, "Version": "v-test"})
	})

	root := httptest.NewRecorder()
	router.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "v-test") {
		t.Fatalf("embedded index response status=%d contains_version=%t", root.Code, strings.Contains(root.Body.String(), "v-test"))
	}
	for _, marker := range []string{
		`data-page="webshell"`,
		`data-page="c2"`,
		`data-page="platform-rbac"`,
		`data-section="robots"`,
		`id="settings-section-terminal"`,
		`/static/js/c2.js`,
		`/static/js/terminal.js`,
		`/static/js/webshell.js`,
		`id="robot-account-binding-modal"`,
	} {
		if strings.Contains(root.Body.String(), marker) {
			t.Fatalf("desktop index contains out-of-scope marker %q", marker)
		}
	}
	if !strings.Contains(root.Body.String(), `/static/js/desktop-setup.js`) {
		t.Fatal("desktop index does not contain desktop setup script")
	}

	server := httptest.NewRecorder()
	router.ServeHTTP(server, httptest.NewRequest(http.MethodGet, "/server", nil))
	if server.Code != http.StatusOK {
		t.Fatalf("server index response status = %d, want 200", server.Code)
	}
	for _, marker := range []string{
		`data-page="webshell"`,
		`data-page="c2"`,
		`data-page="platform-rbac"`,
		`data-section="robots"`,
		`id="settings-section-terminal"`,
		`/static/js/c2.js`,
		`/static/js/terminal.js`,
		`/static/js/webshell.js`,
		`id="robot-account-binding-modal"`,
	} {
		if !strings.Contains(server.Body.String(), marker) {
			t.Fatalf("server index does not contain server-only marker %q", marker)
		}
	}

	coreAsset := httptest.NewRecorder()
	router.ServeHTTP(coreAsset, httptest.NewRequest(http.MethodGet, "/static/js/chat.js", nil))
	if coreAsset.Code != http.StatusOK {
		t.Fatalf("embedded core asset status = %d, want 200", coreAsset.Code)
	}
	desktopSetupAsset := httptest.NewRecorder()
	router.ServeHTTP(desktopSetupAsset, httptest.NewRequest(http.MethodGet, "/static/js/desktop-setup.js", nil))
	if desktopSetupAsset.Code != http.StatusOK {
		t.Fatalf("embedded desktop setup asset status = %d, want 200", desktopSetupAsset.Code)
	}

	excludedAsset := httptest.NewRecorder()
	router.ServeHTTP(excludedAsset, httptest.NewRequest(http.MethodGet, "/static/js/c2.js", nil))
	if excludedAsset.Code != http.StatusNotFound {
		t.Fatalf("excluded desktop asset status = %d, want 404", excludedAsset.Code)
	}
}
