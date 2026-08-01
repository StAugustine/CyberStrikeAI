(function () {
    'use strict';

    const STORAGE_KEY = 'cyberstrike-theme';
    const THEMES = ['system', 'light', 'dark'];
    const FALLBACK_LABELS = {
        system: '跟随系统',
        light: '浅色',
        dark: '暗色'
    };
    const FALLBACK_TITLES = {
        system: '当前：跟随系统主题。点击切换为浅色。',
        light: '当前：浅色主题。点击切换为暗色。',
        dark: '当前：暗色主题。点击切换为跟随系统。'
    };
    const TITLE_KEYS = {
        system: 'titleSystem',
        light: 'titleLight',
        dark: 'titleDark'
    };

    const media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

    function themeText(key, fallback) {
        if (typeof window.t === 'function') {
            const value = window.t('theme.' + key);
            if (value && value !== 'theme.' + key) {
                return value;
            }
        }
        return fallback;
    }

    function getLabel(preference) {
        return themeText(preference, FALLBACK_LABELS[preference] || FALLBACK_LABELS.system);
    }

    function getTitle(preference) {
        const titleKey = TITLE_KEYS[preference] || TITLE_KEYS.system;
        return themeText(titleKey, FALLBACK_TITLES[preference] || FALLBACK_TITLES.system);
    }

    function normalizePreference(value) {
        return THEMES.includes(value) ? value : 'system';
    }

    function readCookiePreference() {
        try {
            const prefix = STORAGE_KEY + '=';
            const entry = document.cookie
                .split(';')
                .map(function (value) { return value.trim(); })
                .find(function (value) { return value.startsWith(prefix); });
            if (!entry) {
                return null;
            }
            const value = decodeURIComponent(entry.slice(prefix.length));
            return THEMES.includes(value) ? value : null;
        } catch (err) {
            return null;
        }
    }

    function readPreference() {
        try {
            const stored = localStorage.getItem(STORAGE_KEY);
            if (THEMES.includes(stored)) {
                return stored;
            }
        } catch (err) {
            // Fall back to the host-scoped cookie below.
        }
        const cookiePreference = readCookiePreference();
        if (cookiePreference) {
            try {
                localStorage.setItem(STORAGE_KEY, cookiePreference);
            } catch (err) {
                // The cookie still keeps the preference across desktop ports.
            }
            return cookiePreference;
        }
        return 'system';
    }

    function resolveTheme(preference) {
        if (preference === 'dark' || preference === 'light') {
            return preference;
        }
        return media && media.matches ? 'dark' : 'light';
    }

    function updateButton(preference, resolved) {
        const btn = document.getElementById('theme-toggle-btn');
        const label = document.getElementById('theme-toggle-label');
        if (!btn) {
            return;
        }
        btn.dataset.themePreference = preference;
        btn.dataset.theme = resolved;
        const title = getTitle(preference);
        btn.title = title;
        btn.setAttribute('aria-label', title);
        if (label) {
            label.textContent = getLabel(preference);
        }
    }

    function applyTheme(preference) {
        const normalized = normalizePreference(preference);
        const resolved = resolveTheme(normalized);
        const root = document.documentElement;
        root.setAttribute('data-theme-preference', normalized);
        root.setAttribute('data-theme', resolved);
        root.style.colorScheme = resolved;
        updateButton(normalized, resolved);
        document.dispatchEvent(
            new CustomEvent('cyberstrike-themechange', {
                detail: { preference: normalized, resolved: resolved },
            }),
        );
    }

    function savePreference(preference) {
        const normalized = normalizePreference(preference);
        try {
            localStorage.setItem(STORAGE_KEY, normalized);
        } catch (err) {
            // Ignore storage failures; the current page can still apply the theme.
        }
        try {
            document.cookie = STORAGE_KEY + '=' + encodeURIComponent(normalized)
                + '; Path=/; Max-Age=31536000; SameSite=Strict';
        } catch (err) {
            // Ignore cookie failures; localStorage still works on stable origins.
        }
        applyTheme(normalized);
    }

    window.setThemePreference = savePreference;
    window.getThemePreference = readPreference;
    window.cycleThemePreference = function () {
        const current = readPreference();
        const next = THEMES[(THEMES.indexOf(current) + 1) % THEMES.length];
        savePreference(next);
    };
    window.refreshThemeToggleLabel = function () {
        applyTheme(readPreference());
    };

    if (media) {
        const onSystemThemeChange = function () {
            if (readPreference() === 'system') {
                applyTheme('system');
            }
        };
        if (typeof media.addEventListener === 'function') {
            media.addEventListener('change', onSystemThemeChange);
        } else if (typeof media.addListener === 'function') {
            media.addListener(onSystemThemeChange);
        }
    }

    document.addEventListener('languagechange', function () {
        applyTheme(readPreference());
    });

    function initTheme() {
        applyTheme(readPreference());
        if (window.i18nReady && typeof window.i18nReady.then === 'function') {
            window.i18nReady.then(function () {
                applyTheme(readPreference());
            });
        }
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', initTheme);
    } else {
        initTheme();
    }
})();
