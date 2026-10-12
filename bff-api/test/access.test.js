import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from '../src/server.js';

async function invoke(path, method, identity) {
  let status, body;
  await createServer('app', { order: 'http://order', payment: 'http://payment', audit: 'http://audit' }, { authenticate: async () => identity })(
    { url: path, method, headers: { 'x-actor-id': 'victim' } }, { writeHead(value) { status = value; }, end(value) { body = JSON.parse(value); } });
  return { status, body };
}
test('another user cannot read order/checkout or confirm a payment; rejection has no mutations', async () => {
  const original = globalThis.fetch;
  const calls = [];
  try {
    globalThis.fetch = async (url, options) => {
      calls.push({ url, method: options.method || 'GET' });
      assert.equal(options.headers['x-actor-id'], 'alice');
      if (url.includes('/v1/payments/')) return Response.json({ id: 'payment', order_id: 'order' });
      if (url.includes('/v1/checkouts/')) return Response.json({ order: { user_id: 'bob' }, state: 'processing' });
      return Response.json({ id: 'order', user_id: 'bob' });
    };
    for (const [path, method] of [['/api/orders/order', 'GET'], ['/api/checkouts/checkout', 'GET'], ['/api/payments/payment/confirm', 'POST']]) {
      assert.equal((await invoke(path, method, { id: 'alice', auditRead: false })).status, 404);
    }
    assert.ok(calls.every(call => call.method === 'GET'));
  } finally { globalThis.fetch = original; }
});
test('audit requires permission before calling the store', async () => {
  const original = globalThis.fetch;
  try {
    globalThis.fetch = async () => assert.fail('unprivileged request reached store');
    assert.equal((await invoke('/api/audit/events', 'GET', { id: 'alice', auditRead: false })).status, 403);
  } finally { globalThis.fetch = original; }
});
