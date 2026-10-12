import { EncryptJWT } from 'jose';
import assert from 'node:assert/strict';
import test, { afterEach } from 'node:test';
import { proxyRequest } from '../lib/bff-proxy.mjs';

const key = Buffer.alloc(32, 7);
Object.assign(process.env, { DEPLOYMENT_ENV: 'development', AUTH_ALLOW_HTTP: 'true', AUTH_ISSUER: 'http://identity', AUTH_JWKS_URL: 'http://identity/keys', AUTH_AUDIENCE: 'api', PUBLIC_URL: 'http://website', OIDC_CLIENT_ID: 'website', OIDC_CLIENT_SECRET: 'test-only', SESSION_SECRET: key.toString('base64url') });
async function sessionHeaders() {
 const value = await new EncryptJWT({ accessToken: 'trusted-access-token', csrf: 'csrf-secret' }).setProtectedHeader({ alg: 'dir', enc: 'A256GCM' }).setIssuer('http://website').setAudience('session').setExpirationTime('5m').encrypt(key);
 return { cookie: `commerce_session=${value}`, origin: 'http://website', 'x-csrf-token': 'csrf-secret' };
}

const originalFetch = globalThis.fetch;
const originalBase = process.env.BFF_URL;
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalBase === undefined) delete process.env.BFF_URL;
  else process.env.BFF_URL = originalBase;
});

test('the same module reads different BFF origins at request time', async () => {
  const destinations = [];
  globalThis.fetch = async url => { destinations.push(String(url)); return Response.json({products: []}); };
  for (const base of ['http://desktop-bff:3001', 'http://other-bff:5001']) {
    process.env.BFF_URL = base;
    await proxyRequest(new Request('http://website/api/audit/events?service=payment', { headers: await sessionHeaders() }), ['audit','events']);
  }
  assert.deepEqual(destinations, ['http://desktop-bff:3001/api/audit/events?service=payment','http://other-bff:5001/api/audit/events?service=payment']);
});

test('proxy preserves checkout body, idempotency key and upstream error status', async () => {
  globalThis.fetch = async (_, options) => {
    assert.equal(options.headers.get('idempotency-key'), 'attempt-1');
    assert.equal(options.headers.get('authorization'), 'Bearer trusted-access-token');
    assert.equal(options.headers.has('x-actor-id'), false);
    assert.equal(await new Response(options.body).text(), '{"items":[]}');
    return Response.json({error:'idempotency_conflict'}, {status:409});
  };
  const response = await proxyRequest(new Request('http://website/api/checkout', {method:'POST',headers:{...await sessionHeaders(), 'content-type':'application/json','idempotency-key':'attempt-1','authorization':'Bearer forged','x-actor-id':'victim'},body:'{"items":[]}'}), ['checkout']);
  assert.equal(response.status,409);
  assert.equal((await response.json()).error,'idempotency_conflict');
});

test('anonymous, foreign-origin and missing CSRF mutations never reach BFF', async () => {
 globalThis.fetch = async () => assert.fail('rejected request reached upstream');
 assert.equal((await proxyRequest(new Request('http://website/api/orders/o'), ['orders','o'])).status,401);
 const headers = await sessionHeaders();
 for (const change of [{ origin: 'https://evil.test' }, { 'x-csrf-token': '' }]) {
   const request = new Request('http://website/api/checkout', {method:'POST',headers:{...headers,...change},body:'{}'});
   assert.equal((await proxyRequest(request,['checkout'])).status,403);
 }
});
