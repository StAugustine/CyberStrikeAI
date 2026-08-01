const assert = require('node:assert/strict');
const fs = require('node:fs');
const test = require('node:test');
const vm = require('node:vm');

const themeScript = fs.readFileSync('web/static/js/theme.js', 'utf8');
const indexTemplate = fs.readFileSync('web/templates/index.html', 'utf8');

function loadTheme(cookieJar, storageValues) {
    const attributes = new Map();
    const localStorage = {
        getItem(key) {
            return storageValues.has(key) ? storageValues.get(key) : null;
        },
        setItem(key, value) {
            storageValues.set(key, String(value));
        },
    };
    const document = {
        readyState: 'complete',
        documentElement: {
            style: {},
            setAttribute(name, value) {
                attributes.set(name, String(value));
            },
        },
        getElementById() {
            return null;
        },
        addEventListener() {},
        dispatchEvent() {},
        get cookie() {
            return cookieJar.value;
        },
        set cookie(value) {
            cookieJar.value = String(value).split(';', 1)[0];
        },
    };
    const window = {
        matchMedia() {
            return { matches: false, addEventListener() {} };
        },
    };
    vm.runInNewContext(themeScript, {
        CustomEvent: function CustomEvent(type, options) {
            this.type = type;
            this.detail = options && options.detail;
        },
        document,
        localStorage,
        window,
    });
    return { attributes, window };
}

test('桌面主题通过 host cookie 跨随机端口恢复', () => {
    const cookieJar = { value: '' };
    const firstPort = loadTheme(cookieJar, new Map());
    firstPort.window.setThemePreference('dark');

    assert.equal(cookieJar.value, 'cyberstrike-theme=dark');

    const secondPortStorage = new Map();
    const secondPort = loadTheme(cookieJar, secondPortStorage);
    assert.equal(secondPort.window.getThemePreference(), 'dark');
    assert.equal(secondPort.attributes.get('data-theme'), 'dark');
    assert.equal(secondPortStorage.get('cyberstrike-theme'), 'dark');
});

test('页面首屏在主题脚本加载前读取 cookie，避免重启后闪回系统主题', () => {
    assert.match(indexTemplate, /document\.cookie\.match\([^\n]+cyberstrike-theme/);
    assert.match(indexTemplate, /stored = match \? decodeURIComponent\(match\[1\]\) : 'system'/);
});
