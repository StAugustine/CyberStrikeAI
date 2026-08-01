/* global chrome */

const CSAI_DESKTOP_NATIVE_HOST = 'com.cyberstrikeai.desktop';
const CSAI_DESKTOP_DISCOVERY_FIELDS = [
  'app_version',
  'base_url',
  'expires_at_unix',
  'instance_id',
  'issued_at_unix',
  'schema_version',
];

function validateDesktopDiscoveryResponse(response, nowUnix = Math.floor(Date.now() / 1000)) {
  if (!response || response.ok !== true || !response.discovery) {
    throw new Error('CyberStrikeAI Desktop integration is unavailable or disabled');
  }
  const responseFields = Object.keys(response).sort();
  if (responseFields.length !== 2 || responseFields[0] !== 'discovery' || responseFields[1] !== 'ok') {
    throw new Error('Desktop discovery response contains unsupported fields');
  }
  const discovery = response.discovery;
  const fields = Object.keys(discovery).sort();
  if (fields.length !== CSAI_DESKTOP_DISCOVERY_FIELDS.length
    || fields.some((field, index) => field !== CSAI_DESKTOP_DISCOVERY_FIELDS[index])) {
    throw new Error('Desktop discovery response contains unsupported fields');
  }
  if (discovery.schema_version !== 1
    || !/^[A-Za-z0-9._-]{16,128}$/.test(discovery.instance_id || '')
    || typeof discovery.app_version !== 'string'
    || !discovery.app_version.trim()
    || discovery.app_version.length > 64) {
    throw new Error('Desktop discovery identity is invalid');
  }
  let endpoint;
  try {
    endpoint = new URL(discovery.base_url);
  } catch (_) {
    throw new Error('Desktop discovery endpoint is invalid');
  }
  if (endpoint.protocol !== 'http:'
    || endpoint.hostname !== '127.0.0.1'
    || !endpoint.port
    || (endpoint.pathname !== '/' && endpoint.pathname !== '')
    || endpoint.search
    || endpoint.hash
    || endpoint.username
    || endpoint.password) {
    throw new Error('Desktop discovery endpoint is invalid');
  }
  const issuedAt = Number(discovery.issued_at_unix);
  const expiresAt = Number(discovery.expires_at_unix);
  if (!Number.isSafeInteger(issuedAt)
    || !Number.isSafeInteger(expiresAt)
    || issuedAt <= 0
    || expiresAt <= issuedAt
    || expiresAt - issuedAt > 120
    || issuedAt > nowUnix + 30
    || expiresAt <= nowUnix) {
    throw new Error('Desktop discovery response is expired or invalid');
  }
  return {
    baseUrl: endpoint.origin,
    host: endpoint.hostname,
    port: endpoint.port,
    https: false,
    instanceId: discovery.instance_id,
    appVersion: discovery.app_version,
    expiresAtUnix: expiresAt,
  };
}

function discoverDesktopInstance() {
  return new Promise((resolve, reject) => {
    if (typeof chrome === 'undefined'
      || !chrome.runtime
      || typeof chrome.runtime.sendNativeMessage !== 'function') {
      reject(new Error('Native messaging is unavailable in this browser'));
      return;
    }
    chrome.runtime.sendNativeMessage(
      CSAI_DESKTOP_NATIVE_HOST,
      { operation: 'discover' },
      (response) => {
        const runtimeError = chrome.runtime.lastError;
        if (runtimeError) {
          reject(new Error('CyberStrikeAI Desktop integration is unavailable or disabled'));
          return;
        }
        try {
          resolve(validateDesktopDiscoveryResponse(response));
        } catch (error) {
          reject(error);
        }
      },
    );
  });
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { validateDesktopDiscoveryResponse };
}
