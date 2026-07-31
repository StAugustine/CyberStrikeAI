package webassets

import (
	"embed"
	"io/fs"
)

// Curated desktop assets intentionally exclude the dedicated C2, Terminal,
// WebShell, robot, multi-user RBAC, and xterm resources.
//
//go:embed templates/*.html
//go:embed static/favicon.ico static/logo.png static/css/style.css static/i18n/*.json
//go:embed static/vendor/cytoscape.min.js static/vendor/elk.bundled.js static/vendor/i18next.min.js static/vendor/marked.min.js static/vendor/purify.min.js static/vendor/xlsx.full.min.js
//go:embed static/js/agents.js static/js/api-docs.js static/js/assets.js static/js/audit-datetime-picker.js static/js/audit.js static/js/auth.js static/js/builtin-tools.js static/js/chat-files.js static/js/chat-scroll.js static/js/chat.js static/js/dashboard.js static/js/desktop-setup.js static/js/fact-graph.js static/js/hitl.js static/js/i18n.js static/js/info-collect.js static/js/knowledge.js static/js/modal.js static/js/monitor.js static/js/notifications.js static/js/projects.js static/js/rbac-guards.js static/js/roles.js static/js/router.js static/js/sanitize-markdown.js static/js/settings.js static/js/skills.js static/js/tasks.js static/js/theme.js static/js/vulnerability.js static/js/workflow-package-client.js static/js/workflows.js
var desktopFiles embed.FS

func FS() fs.FS {
	return desktopFiles
}
