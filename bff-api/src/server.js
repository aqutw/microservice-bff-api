import { randomUUID } from 'node:crypto';
import { createAuthenticator } from './auth.js';
import { apiError, readBody, request } from './http.js';

const defaults = {
  order: process.env.ORDER_API_URL || 'http://localhost:8081',
  inventory: process.env.INVENTORY_API_URL || 'http://localhost:8082',
  payment: process.env.PAYMENT_API_URL || 'http://localhost:8083',
  audit: process.env.AUDIT_API_URL || 'http://localhost:8084'
};

const catalog = [
  { sku: 'coffee-beans', name: '山嵐咖啡豆', unitPriceCents: 420, available: 50 },
  { sku: 'tea-leaves', name: '冷杉烏龍茶', unitPriceCents: 280, available: 30 },
  { sku: 'mug', name: '霧黑陶杯', unitPriceCents: 650, available: 20 }
];

function json(res, status, body) { res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' }); res.end(JSON.stringify(body)); }
function urlPath(req) { return new URL(req.url, 'http://localhost').pathname; }

export async function checkout(input, variant, services = defaults, context = {}) {
  if (!input || typeof input !== 'object' || Array.isArray(input)) throw apiError('invalid_request', 400);
  const userId = context['x-actor-id'];
  if (typeof userId !== 'string' || !userId || userId.length > 128) throw apiError('invalid_user_id', 400);
  const items = Array.isArray(input.items) ? input.items : [];
  if (!items.length) { const e = new Error('items_required'); e.status = 400; throw e; }
  const normalized = items.map(item => {
    const product = catalog.find(product => product.sku === item?.sku);
    if (!product || !Number.isSafeInteger(item.quantity) || item.quantity < 1) { const e = new Error('invalid_items'); e.status = 400; throw e; }
    return { sku: product.sku, quantity: item.quantity, unitPriceCents: product.unitPriceCents };
  });
  const totalCents = normalized.reduce((sum, item) => sum + item.quantity * item.unitPriceCents, 0);
  if (!Number.isSafeInteger(totalCents)) { const e = new Error('invalid_total'); e.status = 400; throw e; }
  const channel = variant === 'app' ? 'app' : 'web';
  const key = context['idempotency-key'] || randomUUID();
  if (typeof key !== 'string' || key.length > 200) throw apiError('invalid_idempotency_key', 400);
  const result = await request(services.order, '/v1/checkouts', {
    method: 'POST', headers: { ...context, 'idempotency-key': key },
    body: JSON.stringify({ user_id: userId, items: normalized.map(({ sku, quantity }) => ({ sku, quantity })), total_cents: totalCents, currency: 'TWD', channel })
  });
  return formatCheckout(result, variant, normalized);
}

function formatCheckout(result, variant, normalized) {
  normalized ||= (result.order?.items || []).map(item => ({ ...item, unitPriceCents: catalog.find(product => product.sku === item.sku)?.unitPriceCents }));
  const cents = result.order?.total_cents ?? normalized.reduce((sum, item) => sum + item.quantity * item.unitPriceCents, 0);
  if (!['awaiting_payment', 'paid'].includes(result.state)) return { checkoutId: result.checkout_id, state: result.state || 'processing' };
  return variant === 'app'
    ? { checkoutId: result.checkout_id, orderId: result.order.id, state: result.state, amount: { cents: cents, currency: 'TWD' }, payment: { id: result.payment.id, clientToken: result.payment.client_token }, reservationId: result.inventory.reservation_id }
    : { checkoutId: result.checkout_id, state: result.state, order: { id: result.order.id, status: result.order.status, total: { cents: cents, currency: 'TWD' }, items: normalized }, payment: { id: result.payment.id, status: result.payment.status, redirectUrl: result.payment.checkout_url }, inventory: { reservationId: result.inventory.reservation_id, status: result.inventory.status } };
}

export function createServer(variant, services = defaults, security = {}) {
  const authenticate = security.authenticate || createAuthenticator();
  const allowedOrigins = new Set((process.env.ALLOWED_ORIGINS || process.env.PUBLIC_URL || '').split(',').filter(Boolean));
  return async (req, res) => {
    const context = { 'x-correlation-id': typeof req.headers['x-correlation-id'] === 'string' && req.headers['x-correlation-id'].length <= 128 ? req.headers['x-correlation-id'] : randomUUID() };
    try {
      const path = urlPath(req);
      const origin = req.headers.origin;
      if (origin && !allowedOrigins.has(origin)) throw apiError('origin_forbidden', 403, false);
      if (origin) { res.setHeader?.('access-control-allow-origin', origin); res.setHeader?.('vary', 'Origin'); }
      if (req.method === 'OPTIONS') { res.writeHead(204, { 'access-control-allow-methods': 'GET,POST,OPTIONS', 'access-control-allow-headers': 'content-type,authorization,idempotency-key,x-correlation-id' }); return res.end(); }
      if (req.method === 'GET' && path === '/healthz') return json(res, 200, { status: 'ok', service: `${variant}-bff` });
      if (req.method === 'GET' && path === '/api/catalog') return json(res, 200, { products: catalog });
      const identity = await authenticate(req);
      context['x-actor-id'] = identity.id;
      context['x-user-scopes'] = identity.auditRead ? 'audit:read' : '';
      if (req.method === 'GET' && path === '/api/audit/events') return json(res, 200, await auditEvents(req, services));
      if (req.method === 'POST' && path === '/api/checkout') {
        const result = await checkout(await readBody(req), variant, services, { ...context, 'idempotency-key': req.headers['idempotency-key'] });
        return json(res, result.state === 'processing' ? 202 : 201, result);
      }
      const checkoutMatch = path.match(/^\/api\/checkouts\/([^/]+)$/);
      if (req.method === 'GET' && checkoutMatch) {
        const result = await request(services.order, `/v1/checkouts/${encodeURIComponent(checkoutMatch[1])}`);
        return json(res, result.state === 'processing' ? 202 : 200, formatCheckout(result, variant));
      }
      const orderMatch = path.match(/^\/api\/orders\/([^/]+)$/);
      if (req.method === 'GET' && orderMatch) return json(res, 200, await request(services.order, `/v1/orders/${encodeURIComponent(orderMatch[1])}`));
      const paymentMatch = path.match(/^\/api\/payments\/([^/]+)\/confirm$/);
      if (req.method === 'POST' && paymentMatch) {
        const payment = await request(services.payment, `/v1/payments/${encodeURIComponent(paymentMatch[1])}/confirm`, { method: 'POST', headers: context, body: '{}' });
        await request(services.order, `/v1/orders/${payment.order_id}`, { method: 'POST', headers: context, body: JSON.stringify({ status: 'paid' }) });
        return json(res, 200, variant === 'app' ? { paymentId: payment.id, state: 'paid', orderId: payment.order_id } : { payment, orderStatus: 'paid' });
      }
      return json(res, 404, { error: 'not_found' });
    } catch (error) { json(res, error.status || 502, { error: error.message || 'bff_error', details: error.body }); }
  };
}

async function auditEvents(req, services) {
  const params = new URL(req.url, 'http://localhost').searchParams;
  const selectedService = params.get('service');
  if (selectedService && !['order', 'inventory', 'payment'].includes(selectedService)) { const error = new Error('invalid_audit_service'); error.status = 400; throw error; }
  const body = await request(services.audit, `/v1/audit/events?${params.toString()}`);
  return { ...body, source: 'audit-worker' };
}
