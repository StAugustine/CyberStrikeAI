const fs = require('node:fs');
const test = require('node:test');
const assert = require('node:assert/strict');

test('桌面首次 AI 向导只在凭据状态缺失时引导配置', () => {
    const html = fs.readFileSync('web/templates/index.html', 'utf8');
    const script = fs.readFileSync('web/static/js/desktop-setup.js', 'utf8');
    assert.match(html, /id="desktop-ai-setup"[\s\S]*?id="desktop-ai-api-key" type="password"/);
    assert.match(html, /src="\/static\/js\/desktop-setup\.js"/);
    assert.match(script, /config\.credential_status\[path\]/);
    assert.match(script, /`ai\.channels\.\$\{defaultID\}\.api_key`/);
});

test('桌面 AI 向导通过受认证配置端点测试、保存和热应用', () => {
    const script = fs.readFileSync('web/static/js/desktop-setup.js', 'utf8');
    assert.match(script, /apiFetch\('\/api\/config\/test-openai'/);
    assert.match(script, /apiFetch\('\/api\/config', \{/);
    assert.match(script, /apiFetch\('\/api\/config\/apply'/);
    assert.match(script, /desktop-ai-api-key'\)\.value = ''/);
    assert.doesNotMatch(script, /localStorage|sessionStorage|URLSearchParams/);
});

test('桌面 AI 向导提供明确的凭据库和失败提示', () => {
    const html = fs.readFileSync('web/templates/index.html', 'utf8');
    const script = fs.readFileSync('web/static/js/desktop-setup.js', 'utf8');
    assert.match(html, /API Key 会写入系统凭据库/);
    assert.match(script, /status === 401/);
    assert.match(script, /status === 429/);
    assert.match(script, /系统凭据库写入失败|保存 AI 通道失败/);
});

test('桌面主窗口只暴露固定的数据和日志目录入口', () => {
    const html = fs.readFileSync('web/templates/index.html', 'utf8');
    const script = fs.readFileSync('web/static/js/desktop-setup.js', 'utf8');
    assert.match(html, /id="desktop-directory-actions" hidden/);
    assert.match(script, /invoke\('open_desktop_directory', \{ directory \}\)/);
    assert.match(script, /window\.__TAURI__\?\.core\?\.invoke/);
});

test('桌面数据维护入口仅打开受限的本地维护窗口', () => {
    const html = fs.readFileSync('web/templates/index.html', 'utf8');
    const script = fs.readFileSync('web/static/js/desktop-setup.js', 'utf8');
    const capability = JSON.parse(fs.readFileSync('desktop/src-tauri/capabilities/data-maintenance.json', 'utf8'));
    const maintenance = fs.readFileSync('desktop/loading/data-maintenance.js', 'utf8');
    assert.match(html, /openDesktopDataMaintenance\(\)[\s\S]*?数据导入与恢复/);
    assert.match(script, /invoke\('open_data_maintenance'\)/);
    assert.deepEqual(capability.windows, ['data-maintenance']);
    assert.doesNotMatch(capability.permissions.join(' '), /shell|execute|spawn/);
    assert.match(maintenance, /invoke\('choose_and_prepare_legacy_import'\)/);
    assert.match(maintenance, /runOfflineOperation\('confirm_legacy_import'/);
    assert.match(maintenance, /runOfflineOperation\('restore_desktop_backup', \{ backupId \}/);
    assert.doesNotMatch(maintenance, /innerHTML|localStorage|sessionStorage|sourceDir/);
});

test('桌面启动错误页提供安全诊断、重试和退出路径', () => {
    const html = fs.readFileSync('desktop/loading/startup-error.html', 'utf8');
    const script = fs.readFileSync('desktop/loading/startup-error.js', 'utf8');
    const capability = JSON.parse(fs.readFileSync('desktop/src-tauri/capabilities/startup-error.json', 'utf8'));
    assert.match(html, /id="retry"/);
    assert.match(html, /id="open-logs"/);
    assert.match(html, /id="open-data"/);
    assert.match(script, /invoke\('get_startup_failure'\)/);
    assert.match(script, /invoke\('retry_startup'\)/);
    assert.match(script, /invoke\('exit_after_startup_failure'\)/);
    assert.doesNotMatch(script, /innerHTML|localStorage|sessionStorage/);
    assert.deepEqual(capability.windows, ['startup-error']);
});
