import assert from 'node:assert/strict';
import test, { afterEach } from 'node:test';
import { proxyRequest } from '../lib/bff-proxy.mjs';

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
    await proxyRequest(new Request('http://website/api/audit/events?service=payment'), ['audit','events']);
  }
  assert.deepEqual(destinations, ['http://desktop-bff:3001/api/audit/events?service=payment','http://other-bff:5001/api/audit/events?service=payment']);
});

test('proxy preserves checkout body, idempotency key and upstream error status', async () => {
  globalThis.fetch = async (_, options) => {
    assert.equal(options.headers.get('idempotency-key'), 'attempt-1');
    assert.equal(await new Response(options.body).text(), '{"items":[]}');
    return Response.json({error:'idempotency_conflict'}, {status:409});
  };
  const response = await proxyRequest(new Request('http://website/api/checkout', {method:'POST',headers:{'content-type':'application/json','idempotency-key':'attempt-1'},body:'{"items":[]}'}), ['checkout']);
  assert.equal(response.status,409);
  assert.equal((await response.json()).error,'idempotency_conflict');
});
