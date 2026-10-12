import assert from 'node:assert/strict';
import test from 'node:test';
import { generateKeyPair, exportJWK, createLocalJWKSet, SignJWT } from 'jose';
import { createAuthenticator, identityConfig } from '../src/auth.js';
import { checkout, createServer } from '../src/server.js';

const { privateKey, publicKey } = await generateKeyPair('RS256');
const keys = createLocalJWKSet({ keys: [{ ...await exportJWK(publicKey), kid: 'one' }] });
const config = { issuer: 'https://identity.test', audience: 'commerce-api', algorithms: ['RS256'] };
const authenticate = createAuthenticator(config, keys);
async function token(claims = {}, key = privateKey) {
  return new SignJWT({ sub: 'alice', iat: Math.floor(Date.now() / 1000), exp: Math.floor(Date.now() / 1000) + 60, iss: config.issuer, aud: config.audience, ...claims }).setProtectedHeader({ alg: 'RS256', kid: 'one' }).sign(key);
}
test('verified identities are stable and separate issuers and subjects', async () => {
  const alice = await authenticate({ headers: { authorization: `Bearer ${await token()}` } });
  const bob = await authenticate({ headers: { authorization: `Bearer ${await token({ sub: 'bob' })}` } });
  assert.match(alice.id, /^usr_[0-9a-f]{64}$/);
  assert.notEqual(alice.id, bob.id);
  assert.equal(alice.auditRead, false);
  assert.equal((await authenticate({ headers: { authorization: `Bearer ${await token({ realm_access: { roles: ['audit-reader'] } })}` } })).auditRead, true);
});
test('missing, expired, wrong issuer/audience, future and forged tokens are rejected', async () => {
  await assert.rejects(authenticate({ headers: {} }), { status: 401 });
  const other = await generateKeyPair('RS256');
  for (const value of [await token({ exp: 1 }), await token({ iss: 'https://attacker.test' }), await token({ aud: 'website' }), await token({ iat: Math.floor(Date.now() / 1000) + 60 }), await token({ typ: 'ID' }), await token({}, other.privateKey)]) {
    await assert.rejects(authenticate({ headers: { authorization: `Bearer ${value}` } }), { status: 401 });
  }
});
test('production configuration fails closed and forbids HTTP/HS256', () => {
  assert.throws(() => identityConfig({}));
  const env = { AUTH_ISSUER: 'http://identity', AUTH_JWKS_URL: 'http://identity/keys', AUTH_AUDIENCE: 'api', AUTH_ALLOW_HTTP: 'true' };
  assert.throws(() => identityConfig(env));
  assert.doesNotThrow(() => identityConfig({ ...env, DEPLOYMENT_ENV: 'development' }));
  assert.throws(() => identityConfig({ ...env, DEPLOYMENT_ENV: 'development', AUTH_ALGORITHMS: 'HS256' }));
});
test('unverified request cannot reach an upstream and catalog stays public', async () => {
  const handler = createServer('app', {}, { authenticate });
  let status;
  const response = { writeHead(value) { status = value; }, end() {} };
  await handler({ method: 'POST', url: '/api/checkout', headers: {} }, response);
  assert.equal(status, 401);
  await handler({ method: 'GET', url: '/api/catalog', headers: {} }, response);
  assert.equal(status, 200);
});
test('checkout ignores a forged owner in the request body', async () => {
  const original = globalThis.fetch;
  try {
    globalThis.fetch = async (_, options) => {
      assert.equal(JSON.parse(options.body).user_id, 'trusted-user');
      return Response.json({ checkout_id: 'c', state: 'processing' });
    };
    await checkout({ user_id: 'victim', items: [{ sku: 'mug', quantity: 1 }] }, 'app', { order: 'http://order' }, { 'x-actor-id': 'trusted-user' });
  } finally { globalThis.fetch = original; }
});
