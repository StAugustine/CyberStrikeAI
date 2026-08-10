const assert = require('node:assert/strict');
const fs = require('node:fs');
const test = require('node:test');

const dashboardScript = fs.readFileSync('web/static/js/dashboard.js', 'utf8');
const indexTemplate = fs.readFileSync('web/templates/index.html', 'utf8');

test('桌面仪表盘不渲染或轮询已排除的 C2 与 WebShell 概览', () => {
    const accessSection = indexTemplate.indexOf('id="dashboard-section-access"');
    assert.ok(accessSection > 0);
    assert.match(indexTemplate.slice(accessSection - 260, accessSection), /\{\{if not \.DesktopMode\}\}/);

    assert.match(dashboardScript, /var accessOverviewEnabled = !!document\.getElementById\('dashboard-section-access'\)/);
    assert.match(dashboardScript, /var fetchAccessJson = accessOverviewEnabled \? fetchJson/);
    assert.match(dashboardScript, /fetchAccessJson\(dashboardProjectScopedUrl\('\/api\/webshell\/connections'\)\)/);
    assert.match(dashboardScript, /fetchAccessJson\(dashboardProjectScopedUrl\('\/api\/c2\/listeners'\)\)/);
    assert.match(dashboardScript, /fetchAccessJson\(dashboardProjectScopedUrl\('\/api\/c2\/sessions\?limit=500'\)\)/);
    assert.match(dashboardScript, /fetchAccessJson\(dashboardProjectScopedUrl\('\/api\/c2\/tasks\?page=1&page_size=1'\)\)/);
});
