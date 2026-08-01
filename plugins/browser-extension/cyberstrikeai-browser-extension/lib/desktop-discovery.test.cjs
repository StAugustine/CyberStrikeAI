const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const { validateDesktopDiscoveryResponse } = require('./desktop-discovery.js');

const now = 1_800_000_000;

function response(overrides = {}) {
  return {
    ok: true,
    discovery: {
      schema_version: 1,
      instance_id: 'desktop-instance-123456',
      base_url: 'http://127.0.0.1:43123',
      app_version: '0.1.0',
      issued_at_unix: now,
      expires_at_unix: now + 90,
      ...overrides,
    },
  };
}

test('desktop discovery accepts only short-lived loopback metadata', () => {
  assert.deepEqual(validateDesktopDiscoveryResponse(response(), now), {
    baseUrl: 'http://127.0.0.1:43123',
    host: '127.0.0.1',
    port: '43123',
    https: false,
    instanceId: 'desktop-instance-123456',
    appVersion: '0.1.0',
    expiresAtUnix: now + 90,
  });
  assert.throws(() => validateDesktopDiscoveryResponse(response({ base_url: 'http://192.0.2.1:43123' }), now), /endpoint/);
  assert.throws(() => validateDesktopDiscoveryResponse(response({ expires_at_unix: now }), now), /expired/);
  assert.throws(() => validateDesktopDiscoveryResponse(response({ expires_at_unix: now + 121 }), now), /expired/);
});

test('desktop discovery rejects tokens and unknown fields even from a compromised host', () => {
  assert.throws(
    () => validateDesktopDiscoveryResponse(response({ token: 'must-not-be-accepted' }), now),
    /unsupported fields/,
  );
  assert.throws(
    () => validateDesktopDiscoveryResponse({ ...response(), token: 'must-not-be-accepted' }, now),
    /unsupported fields/,
  );
  assert.throws(
    () => validateDesktopDiscoveryResponse({ ok: false, error: 'disabled' }, now),
    /unavailable or disabled/,
  );
});

test('manifest public key pins the official desktop extension identity', () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'manifest.json'), 'utf8'));
  const digest = crypto.createHash('sha256').update(Buffer.from(manifest.key, 'base64')).digest().subarray(0, 16);
  const extensionId = Array.from(digest)
    .flatMap((byte) => [byte >> 4, byte & 0x0f])
    .map((nibble) => String.fromCharCode('a'.charCodeAt(0) + nibble))
    .join('');
  assert.equal(extensionId, 'okialefpaaimfgjelpednbehgebgkdgo');
  assert.ok(manifest.permissions.includes('nativeMessaging'));
  assert.deepEqual(manifest.host_permissions, ['http://127.0.0.1/*']);
});
