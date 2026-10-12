import assert from 'node:assert/strict';
import test, { afterEach } from 'node:test';
import { checkout, createServer } from '../src/server.js';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function response(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body) };
}

function invoke(handler, request) {
  return new Promise((resolve, reject) => {
    const output = { status: 200, headers: {} };
    const result = {
      writeHead(status, headers) { output.status = status; output.headers = headers; },
      end(body) { output.body = body ? JSON.parse(body) : null; resolve(output); }
    };
    Promise.resolve(handler(request, result)).catch(reject);
  });
}

test('desktop checkout keeps the Web response contract', async () => {
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (url.endsWith('/v1/orders')) return response({ id: 'ord-1', status: 'pending_payment' }, 201);
    if (url.endsWith('/v1/reservations')) return response({ reservation_id: 'rsv-1', status: 'reserved' }, 201);
    return response({ id: 'pay-1', order_id: 'ord-1', status: 'pending', checkout_url: 'http://pay.test' }, 201);
  };
  const result = await checkout({ user_id: 'user-1', items: [{ sku: 'mug', quantity: 1, unit_price_cents: 650 }] }, 'desktop', { order: 'http://order', inventory: 'http://inventory', payment: 'http://payment' });
  assert.equal(calls.length, 3);
  assert.equal(result.order.id, 'ord-1');
  assert.equal(result.payment.redirectUrl, 'http://pay.test');
  assert.equal(result.inventory.reservationId, 'rsv-1');
  assert.equal(result.order.total.cents, 650);
});

test('app checkout keeps the App response contract', async () => {
  globalThis.fetch = async url => {
    if (url.endsWith('/v1/orders')) return response({ id: 'ord-2', status: 'pending_payment' }, 201);
    if (url.endsWith('/v1/reservations')) return response({ reservation_id: 'rsv-2', status: 'reserved' }, 201);
    return response({ id: 'pay-2', order_id: 'ord-2', status: 'pending', client_token: 'token-2' }, 201);
  };
  const result = await checkout({ user_id: 'user-2', items: [{ sku: 'mug', quantity: 1, unit_price_cents: 650 }] }, 'app', { order: 'http://order', inventory: 'http://inventory', payment: 'http://payment' });
  assert.equal(result.state, 'awaiting_payment');
  assert.equal(result.payment.clientToken, 'token-2');
  assert.equal(result.reservationId, 'rsv-2');
});

test('audit BFF reads the centralized audit API', async () => {
  globalThis.fetch = async url => {
    assert.match(url, /^http:\/\/audit:8084\/v1\/audit\/events\?service=payment&limit=20$/);
    return response({ service: 'audit-worker', events: [{ event_id: 'evt-1', service: 'payment' }], count: 1 });
  };
  const result = await invoke(createServer('desktop', { audit: 'http://audit:8084' }), { method: 'GET', url: '/api/audit/events?service=payment&limit=20', headers: {} });
  assert.equal(result.status, 200);
  assert.equal(result.body.source, 'audit-worker');
  assert.equal(result.body.events[0].event_id, 'evt-1');
});

test('audit API failure is isolated from checkout', async () => {
  globalThis.fetch = async url => {
    if (url.includes('/v1/audit/events')) return response({ error: 'unavailable' }, 503);
    if (url.endsWith('/v1/orders')) return response({ id: 'ord-3', status: 'pending_payment' }, 201);
    if (url.endsWith('/v1/reservations')) return response({ reservation_id: 'rsv-3', status: 'reserved' }, 201);
    return response({ id: 'pay-3', order_id: 'ord-3', status: 'pending', checkout_url: 'http://pay.test' }, 201);
  };
  const auditResult = await invoke(createServer('desktop', { audit: 'http://audit:8084' }), { method: 'GET', url: '/api/audit/events', headers: {} });
  assert.equal(auditResult.status, 503);
  const checkoutResult = await checkout({ user_id: 'user-3', items: [{ sku: 'mug', quantity: 1, unit_price_cents: 650 }] }, 'desktop', { order: 'http://order', inventory: 'http://inventory', payment: 'http://payment', audit: 'http://audit:8084' });
  assert.equal(checkoutResult.order.id, 'ord-3');
});

test('server catalog controls prices regardless of client-supplied amounts', async () => {
  const payments = [];
  globalThis.fetch = async (url, options = {}) => {
    if (url.endsWith('/v1/orders')) return response({ id: 'priced', status: 'pending_payment' }, 201);
    if (url.endsWith('/v1/reservations')) return response({ reservation_id: 'reserved', status: 'reserved' }, 201);
    payments.push(JSON.parse(options.body));
    return response({ id: 'pay', checkout_url: 'demo' }, 201);
  };
  for (const price of [0, -1, 1, 999999]) {
    const result = await checkout({ items: [{ sku: 'mug', quantity: 2, unit_price_cents: price }] }, 'desktop');
    assert.equal(result.order.total.cents, 1300);
  }
  assert.ok(payments.every(payment => payment.amount_cents === 1300));
});

test('invalid SKUs, quantities, and overflowing totals never create an order', async () => {
  globalThis.fetch = async () => { assert.fail('invalid input reached upstream'); };
  for (const item of [null, { sku: 'missing', quantity: 1 }, { sku: 'mug', quantity: '1' }, { sku: 'mug', quantity: 0 }, { sku: 'mug', quantity: 1.5 }, { sku: 'mug', quantity: Number.MAX_SAFE_INTEGER }]) {
    await assert.rejects(checkout({ items: [item] }, 'desktop'), error => error.status === 400);
  }
});
