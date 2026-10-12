import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after } from 'node:test';
import { createStoredSession, revokeSession } from '../lib/auth.mjs';
const sessionDirectory = mkdtempSync(join(tmpdir(),'commerce-session-test-'));
after(() => rmSync(sessionDirectory,{recursive:true,force:true}));
import assert from 'node:assert/strict';
import test from 'node:test';
import { EncryptJWT } from 'jose';
import { authSettings, readSession, validMutation, authRoute } from '../lib/auth.mjs';

const env = { SESSION_STORE_DIR: sessionDirectory, DEPLOYMENT_ENV: 'development', AUTH_ALLOW_HTTP: 'true', AUTH_ISSUER: 'http://identity', AUTH_JWKS_URL: 'http://identity/keys', AUTH_AUDIENCE: 'api', PUBLIC_URL: 'http://website', OIDC_CLIENT_ID: 'website', OIDC_CLIENT_SECRET: 'test-only', SESSION_SECRET: Buffer.alloc(32, 3).toString('base64url') };
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

test('production sessions require a host-prefixed cookie and malformed CSRF is denied', async () => {
 const production = authSettings({...env,DEPLOYMENT_ENV:'production',PUBLIC_URL:'https://website',AUTH_ISSUER:'https://identity',AUTH_JWKS_URL:'https://identity/keys'});
 const value = await createStoredSession({accessToken:'token',csrf:'secret'},production,60);
 assert.equal(await readSession(new Request('https://website',{headers:{cookie:`commerce_session=${value}`}}),production),null);
 assert.equal((await readSession(new Request('https://website',{headers:{cookie:`__Host-commerce_session=${value}`}}),production)).accessToken,'token');
 assert.equal(validMutation(new Request('https://website',{headers:{origin:'https://website','x-csrf-token':'üüüüüü'}}),{csrf:'secret'},production),false);
});

test('logout invalidates retained copies of a session cookie',async()=>{
 const value = await createStoredSession({accessToken:'token',csrf:'secret'},settings,60);
 const request = new Request('http://website',{headers:{cookie:`commerce_session=${value}`}});
 const session = await readSession(request,settings);assert.equal(session.accessToken,'token');
 await revokeSession(session,settings);assert.equal(await readSession(request,settings),null);
});
