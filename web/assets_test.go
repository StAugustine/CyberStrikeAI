package webassets

import (
	"io/fs"
	"testing"
)

func TestDesktopAssetsContainCoreResourcesAndExcludeOutOfScopeModules(t *testing.T) {
	for _, path := range []string{
		"templates/index.html",
		"templates/api-docs.html",
		"static/css/style.css",
		"static/js/chat.js",
		"static/js/desktop-setup.js",
		"static/js/knowledge.js",
		"static/js/workflows.js",
	} {
		if _, err := fs.Stat(FS(), path); err != nil {
			t.Fatalf("required desktop asset %q: %v", path, err)
		}
	}

	for _, path := range []string{
		"static/css/c2.css",
		"static/js/c2.js",
		"static/js/terminal.js",
		"static/js/webshell.js",
		"static/js/wechat-robot.js",
		"static/js/rbac.js",
		"static/vendor/xterm.js",
		"static/vendor/xterm-addon-fit.js",
		"static/vendor/xterm.css",
	} {
		if _, err := fs.Stat(FS(), path); err == nil {
			t.Fatalf("out-of-scope desktop asset %q was embedded", path)
		}
	}
}
