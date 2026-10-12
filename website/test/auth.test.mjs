import assert from 'node:assert/strict';
import test from 'node:test';
import { EncryptJWT } from 'jose';
import { authSettings, readSession, validMutation, authRoute } from '../lib/auth.mjs';

const env = { DEPLOYMENT_ENV: 'development', AUTH_ALLOW_HTTP: 'true', AUTH_ISSUER: 'http://identity', AUTH_JWKS_URL: 'http://identity/keys', AUTH_AUDIENCE: 'api', PUBLIC_URL: 'http://website', OIDC_CLIENT_ID: 'website', OIDC_CLIENT_SECRET: 'test-only', SESSION_SECRET: Buffer.alloc(32, 3).toString('base64url') };
Object.assign(process.env, env);
const settings = authSettings(env);
test('production authentication requires TLS and an encryption key', () => {
  assert.throws(() => authSettings({ ...env, DEPLOYMENT_ENV: 'production' }));
  assert.throws(() => authSettings({ ...env, SESSION_SECRET: '' }));
});
test('session rejects expired, tampered and different-purpose cookies', async () => {
  for (const [purpose, expiration] of [['session', 1], ['login', '5m']]) {
    const value = await new EncryptJWT({ accessToken: 'token', csrf: 'secret' }).setProtectedHeader({ alg: 'dir', enc: 'A256GCM' }).setIssuer('http://website').setAudience(purpose).setExpirationTime(expiration).encrypt(settings.key);
    assert.equal(await readSession(new Request('http://website', { headers: { cookie: `commerce_session=${value}` } }), settings), null);
  }
  assert.equal(await readSession(new Request('http://website', { headers: { cookie: 'commerce_session=forged' } }), settings), null);
});
test('CSRF requires both the configured origin and session-bound token', async () => {
  const session = { csrf: 'secret' };
  assert.equal(validMutation(new Request('http://website', { headers: { origin: 'http://website', 'x-csrf-token': 'secret' } }), session, settings), true);
  for (const headers of [{ origin: 'http://website' }, { origin: 'http://evil', 'x-csrf-token': 'secret' }, { 'x-csrf-token': 'secret' }]) assert.equal(validMutation(new Request('http://website', { headers }), session, settings), false);
  assert.equal((await authRoute(new Request('http://website/auth/logout', { method: 'POST' }), 'logout')).status, 401);
});
