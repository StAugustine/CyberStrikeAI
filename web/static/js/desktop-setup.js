let desktopAISetupChecked = false;
let desktopAISetupConfig = null;

function normalizeDesktopAIChannelID(value) {
    const normalized = String(value || '').trim().toLowerCase().replace(/_/g, '-');
    const id = normalized.replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    return id || 'default';
}

function setDesktopAISetupMessage(text, state = '') {
    const element = document.getElementById('desktop-ai-setup-message');
    if (!element) return;
    element.textContent = text || '';
    element.dataset.state = state;
}

function setDesktopAISetupBusy(busy) {
    ['desktop-ai-test', 'desktop-ai-save', 'desktop-ai-later'].forEach((id) => {
        const button = document.getElementById(id);
        if (button) button.disabled = busy;
    });
}

function desktopAISetupValues() {
    return {
        provider: document.getElementById('desktop-ai-provider')?.value || 'openai_compatible',
        base_url: document.getElementById('desktop-ai-base-url')?.value.trim() || '',
        api_key: document.getElementById('desktop-ai-api-key')?.value.trim() || '',
        model: document.getElementById('desktop-ai-model')?.value.trim() || ''
    };
}

function validateDesktopAISetup(values) {
    if (!values.base_url) return '请填写 Base URL。';
    if (!values.api_key) return '请填写 API Key。';
    if (!values.model) return '请填写模型名称。';
    return '';
}

function closeDesktopAISetup() {
    const overlay = document.getElementById('desktop-ai-setup');
    const keyInput = document.getElementById('desktop-ai-api-key');
    if (keyInput) keyInput.value = '';
    if (overlay) overlay.hidden = true;
}

async function openDesktopDirectory(directory) {
    if (!window.__TAURI__?.core?.invoke) return;
    try {
        await window.__TAURI__.core.invoke('open_desktop_directory', { directory });
        if (typeof setUserMenuOpen === 'function') setUserMenuOpen(false);
    } catch (error) {
        if (typeof showNotification === 'function') {
            showNotification(typeof error === 'string' ? error : '无法打开桌面目录', 'error');
        }
    }
}

async function openDesktopDataMaintenance() {
    if (!window.__TAURI__?.core?.invoke) return;
    try {
        await window.__TAURI__.core.invoke('open_data_maintenance');
        if (typeof setUserMenuOpen === 'function') setUserMenuOpen(false);
    } catch (error) {
        if (typeof showNotification === 'function') {
            showNotification(typeof error === 'string' ? error : '无法打开数据维护窗口', 'error');
        }
    }
}

async function manageDesktopPluginIntegration() {
    if (!window.__TAURI__?.core?.invoke) return;
    try {
        const status = await window.__TAURI__.core.invoke('get_plugin_integration_status');
        const enabling = !status.enabled;
        const prompt = enabling
            ? '启用后，Chrome/Edge 与 Burp 只能发现当前本地实例的短期地址；仍需本地管理员登录，发现信息不包含密码或令牌。是否启用？'
            : '禁用后将删除本地发现信息并注销浏览器原生消息宿主。是否继续？';
        if (!window.confirm(prompt)) return;
        const updated = await window.__TAURI__.core.invoke('set_plugin_integration_enabled', { enabled: enabling });
        const message = updated.enabled
            ? (updated.browser_registered
                ? '本地插件联动已启用；请在浏览器扩展或 Burp 中选择“Use Desktop”。'
                : '本地发现已启用，但浏览器原生消息注册失败；Burp 仍可使用发现文件。')
            : '本地插件联动已禁用。';
        if (typeof showNotification === 'function') showNotification(message, updated.enabled ? 'success' : 'info');
        if (typeof setUserMenuOpen === 'function') setUserMenuOpen(false);
    } catch (error) {
        if (typeof showNotification === 'function') {
            showNotification(typeof error === 'string' ? error : '无法更新本地插件联动', 'error');
        }
    }
}

function initializeDesktopDirectoryActions() {
    const actions = document.getElementById('desktop-directory-actions');
    if (actions && window.__TAURI__?.core?.invoke) actions.hidden = false;
}

async function maybeShowDesktopAISetup() {
    if (desktopAISetupChecked || typeof apiFetch !== 'function') return;
    if (typeof hasPermission === 'function' && !hasPermission('config:write')) {
        desktopAISetupChecked = true;
        return;
    }
    try {
        const response = await apiFetch('/api/config');
        if (!response.ok) return;
        const config = await response.json();
        if (!config.credential_status || typeof config.credential_status !== 'object') {
            desktopAISetupChecked = true;
            return;
        }
        desktopAISetupChecked = true;
        desktopAISetupConfig = config;
        const defaultID = normalizeDesktopAIChannelID(config.ai?.default_channel);
        const path = `ai.channels.${defaultID}.api_key`;
        if (config.credential_status[path]) return;

        const channel = config.ai?.channels?.[defaultID] || config.openai || {};
        const provider = document.getElementById('desktop-ai-provider');
        const baseURL = document.getElementById('desktop-ai-base-url');
        const model = document.getElementById('desktop-ai-model');
        if (provider) provider.value = channel.provider === 'claude' ? 'claude' : 'openai_compatible';
        if (baseURL) baseURL.value = channel.base_url || (provider?.value === 'claude' ? 'https://api.anthropic.com' : 'https://api.openai.com/v1');
        if (model) model.value = channel.model || '';
        const overlay = document.getElementById('desktop-ai-setup');
        if (overlay) overlay.hidden = false;
        setTimeout(() => document.getElementById('desktop-ai-api-key')?.focus(), 50);
    } catch (error) {
        console.warn('桌面 AI 首次设置检查失败:', error);
    }
}

async function testDesktopAIConnection() {
    const values = desktopAISetupValues();
    const validation = validateDesktopAISetup(values);
    if (validation) {
        setDesktopAISetupMessage(validation, 'error');
        return;
    }
    setDesktopAISetupBusy(true);
    setDesktopAISetupMessage('正在测试连接…', 'pending');
    try {
        const response = await apiFetch('/api/config/test-openai', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(values)
        });
        const result = await response.json().catch(() => ({}));
        if (!response.ok || !result.success) {
            const status = result.status_code;
            const hint = status === 401
                ? '认证失败，请检查 API Key。'
                : status === 429
                    ? '服务商限流或额度不足，请稍后重试。'
                    : (result.error || '连接失败，请检查地址和模型。');
            throw new Error(hint);
        }
        const latency = result.latency_ms ? `（${result.latency_ms} ms）` : '';
        setDesktopAISetupMessage(`连接成功${latency}`, 'success');
    } catch (error) {
        setDesktopAISetupMessage(error.message || '连接测试失败。', 'error');
    } finally {
        setDesktopAISetupBusy(false);
    }
}

async function saveDesktopAISetup(event) {
    event.preventDefault();
    const values = desktopAISetupValues();
    const validation = validateDesktopAISetup(values);
    if (validation) {
        setDesktopAISetupMessage(validation, 'error');
        return;
    }
    const config = desktopAISetupConfig || {};
    const defaultID = normalizeDesktopAIChannelID(config.ai?.default_channel);
    const previousChannel = config.ai?.channels?.[defaultID] || {};
    const channels = { ...(config.ai?.channels || {}) };
    channels[defaultID] = {
        ...previousChannel,
        name: previousChannel.name || 'Default',
        provider: values.provider,
        base_url: values.base_url,
        api_key: values.api_key,
        model: values.model,
        max_total_tokens: previousChannel.max_total_tokens || 120000,
        max_completion_tokens: previousChannel.max_completion_tokens || 32768
    };

    setDesktopAISetupBusy(true);
    setDesktopAISetupMessage('正在写入系统凭据库…', 'pending');
    try {
        const update = await apiFetch('/api/config', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                ai: { default_channel: defaultID, channels }
            })
        });
        if (!update.ok) {
            const body = await update.json().catch(() => ({}));
            throw new Error(body.error || '保存 AI 通道失败。');
        }
        document.getElementById('desktop-ai-api-key').value = '';
        const apply = await apiFetch('/api/config/apply', { method: 'POST' });
        if (!apply.ok) {
            throw new Error('凭据已保存，但热应用失败；重启桌面客户端后会生效。');
        }
        closeDesktopAISetup();
        if (typeof showNotification === 'function') {
            showNotification('AI 通道已保存到系统凭据库', 'success');
        }
        if (typeof initChatAgentModeFromConfig === 'function') {
            await initChatAgentModeFromConfig();
        }
    } catch (error) {
        setDesktopAISetupMessage(error.message || '保存 AI 通道失败。', 'error');
        setDesktopAISetupBusy(false);
    }
}

document.getElementById('desktop-ai-test')?.addEventListener('click', testDesktopAIConnection);
document.getElementById('desktop-ai-setup-form')?.addEventListener('submit', saveDesktopAISetup);
document.getElementById('desktop-ai-later')?.addEventListener('click', closeDesktopAISetup);
window.maybeShowDesktopAISetup = maybeShowDesktopAISetup;
window.openDesktopDirectory = openDesktopDirectory;
window.openDesktopDataMaintenance = openDesktopDataMaintenance;
window.manageDesktopPluginIntegration = manageDesktopPluginIntegration;
initializeDesktopDirectoryActions();
