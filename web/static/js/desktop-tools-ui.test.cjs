const assert = require('node:assert/strict');
const fs = require('node:fs');
const test = require('node:test');

const settingsScript = fs.readFileSync('web/static/js/settings.js', 'utf8');
const zh = JSON.parse(fs.readFileSync('web/static/i18n/zh-CN.json', 'utf8'));
const en = JSON.parse(fs.readFileSync('web/static/i18n/en-US.json', 'utf8'));

test('桌面工具诊断区分定义、内置运行时和系统命令可用性', () => {
    assert.match(settingsScript, /tool\.runtime_kind === 'system_command'/);
    assert.match(settingsScript, /tool\.executable_available === true/);
    assert.match(settingsScript, /tool\.executable_path \|\| tool\.executable_command/);
    assert.match(settingsScript, /tool\.runtime_kind === 'builtin'/);
    assert.match(settingsScript, /tool-runtime-badge \$\{executableAvailable \? 'available' : 'missing'\}/);

    for (const dictionary of [zh, en]) {
        assert.ok(dictionary.mcp.definitionInstalled);
        assert.ok(dictionary.mcp.executableAvailable);
        assert.ok(dictionary.mcp.executableMissing);
        assert.ok(dictionary.mcp.builtinRuntime);
    }
});
