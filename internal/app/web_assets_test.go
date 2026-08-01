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
		`id="dashboard-section-access"`,
		`window.open('/api-docs'`,
		`window.open('https://github.com/Ed1s0nZ/CyberStrikeAI'`,
	} {
		if strings.Contains(root.Body.String(), marker) {
			t.Fatalf("desktop index contains out-of-scope marker %q", marker)
		}
	}
	if !strings.Contains(root.Body.String(), `/static/js/desktop-setup.js`) {
		t.Fatal("desktop index does not contain desktop setup script")
	}
	if !strings.Contains(root.Body.String(), `window.location.assign('/api-docs')`) {
		t.Fatal("desktop index does not contain the local API documentation entry")
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
		`id="dashboard-section-access"`,
		`window.open('/api-docs'`,
		`window.open('https://github.com/Ed1s0nZ/CyberStrikeAI'`,
	} {
		if !strings.Contains(server.Body.String(), marker) {
			t.Fatalf("server index does not contain server-only marker %q", marker)
		}
	}

	for _, path := range []string{
		"/static/i18n/en-US.json",
		"/static/i18n/zh-CN.json",
		"/static/vendor/marked.min.js",
		"/static/vendor/purify.min.js",
		"/static/js/chat.js",
		"/static/js/i18n.js",
		"/static/js/monitor.js",
		"/static/js/notifications.js",
		"/static/js/sanitize-markdown.js",
		"/static/js/theme.js",
	} {
		asset := httptest.NewRecorder()
		router.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, path, nil))
		if asset.Code != http.StatusOK {
			t.Fatalf("embedded core asset %s status = %d, want 200", path, asset.Code)
		}
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
