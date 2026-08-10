const assert = require('node:assert/strict');
const fs = require('node:fs');
const test = require('node:test');

const authScript = fs.readFileSync('web/static/js/auth.js', 'utf8');

test('桌面会话仅保留到当前 WebView 会话，浏览器保持原有持久化行为', () => {
    assert.match(authScript, /window\.__TAURI__\?\.core\?\.invoke/);
    assert.match(authScript, /return window\.sessionStorage/);
    assert.match(authScript, /return window\.localStorage/);
    assert.match(authScript, /getAuthStorage\(\)\.setItem\(AUTH_STORAGE_KEY/);
    assert.match(authScript, /getAuthStorage\(\)\.removeItem\(AUTH_STORAGE_KEY/);
});

test('桌面认证存储不包含登录密码或 AI 凭据', () => {
    const start = authScript.indexOf('function saveAuth(');
    const end = authScript.indexOf('function clearAuthStorage(');
    assert.ok(start >= 0 && end > start);
    const saveAuth = authScript.slice(start, end);
    assert.doesNotMatch(saveAuth, /password|api[_-]?key/i);
});

test('刷新校验、Bearer 注入、401 清理和退出撤销路径保持完整', () => {
    assert.match(authScript, /apiFetch\('\/api\/auth\/validate'/);
    assert.match(authScript, /headers\.set\('Authorization', `Bearer \$\{authToken\}`\)/);
    assert.match(authScript, /response\.status === 401/);
    assert.match(authScript, /handleUnauthorized\(\)/);
    assert.match(authScript, /fetch\('\/api\/auth\/logout'/);
    assert.match(authScript, /finally \{[\s\S]*clearAuthStorage\(\)/);
});
