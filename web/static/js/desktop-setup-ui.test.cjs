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
