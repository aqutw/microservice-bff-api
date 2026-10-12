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

async function request(base, path, options = {}) {
  const response = await fetch(`${base}${path}`, {
    ...options,
    headers: { 'content-type': 'application/json', ...(options.headers || {}) }
  });
  const text = await response.text();
  let body; try { body = text ? JSON.parse(text) : {}; } catch { body = { error: text }; }
  if (!response.ok) { const error = new Error(body.error || `upstream_${response.status}`); error.status = response.status; error.body = body; throw error; }
  return body;
}

function json(res, status, body) { res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'access-control-allow-origin': '*' }); res.end(JSON.stringify(body)); }
function readBody(req) { return new Promise((resolve, reject) => { let raw = ''; req.on('data', c => { raw += c; if (raw.length > 1024 * 1024) reject(new Error('payload_too_large')); }); req.on('end', () => { try { resolve(raw ? JSON.parse(raw) : {}); } catch { reject(new Error('invalid_json')); } }); req.on('error', reject); }); }
function urlPath(req) { return new URL(req.url, 'http://localhost').pathname; }

export async function checkout(input, variant, services = defaults, context = {}) {
  const userId = input.user_id || input.userId || 'demo-user';
  const items = Array.isArray(input.items) ? input.items : [];
  if (!items.length) { const e = new Error('items_required'); e.status = 400; throw e; }
  const normalized = items.map(item => ({ sku: String(item.sku), quantity: Number(item.quantity), unitPriceCents: Number(item.unit_price_cents ?? item.unitPriceCents ?? catalog.find(p => p.sku === item.sku)?.unitPriceCents ?? 0) }));
  if (normalized.some(item => !item.sku || !Number.isInteger(item.quantity) || item.quantity < 1 || !Number.isInteger(item.unitPriceCents) || item.unitPriceCents < 0)) { const e = new Error('invalid_items'); e.status = 400; throw e; }
  const totalCents = normalized.reduce((sum, item) => sum + item.quantity * item.unitPriceCents, 0);
  const channel = variant === 'app' ? 'app' : 'web';
  const order = await request(services.order, '/v1/orders', { method: 'POST', headers: context, body: JSON.stringify({ user_id: userId, items: normalized.map(({ sku, quantity }) => ({ sku, quantity })), total_cents: totalCents, currency: 'TWD' }) });
  let reservation;
  try {
    reservation = await request(services.inventory, '/v1/reservations', { method: 'POST', headers: context, body: JSON.stringify({ order_id: order.id, items: normalized.map(({ sku, quantity }) => ({ sku, quantity })) }) });
  } catch (error) {
    await request(services.order, `/v1/orders/${order.id}`, { method: 'POST', headers: context, body: JSON.stringify({ status: 'cancelled' }) }).catch(() => {});
    throw error;
  }
  let payment;
  try {
    payment = await request(services.payment, '/v1/payments', { method: 'POST', headers: context, body: JSON.stringify({ order_id: order.id, amount_cents: totalCents, currency: 'TWD', channel }) });
  } catch (error) {
    await request(services.inventory, `/v1/reservations/${reservation.reservation_id}/release`, { method: 'POST', headers: context }).catch(() => {});
    await request(services.order, `/v1/orders/${order.id}`, { method: 'POST', headers: context, body: JSON.stringify({ status: 'cancelled' }) }).catch(() => {});
    throw error;
  }
  return variant === 'app'
    ? { orderId: order.id, state: 'awaiting_payment', amount: { cents: totalCents, currency: 'TWD' }, payment: { id: payment.id, clientToken: payment.client_token }, reservationId: reservation.reservation_id }
    : { order: { id: order.id, status: order.status, total: { cents: totalCents, currency: 'TWD' }, items: normalized.map(item => ({ sku: item.sku, quantity: item.quantity, unitPriceCents: item.unitPriceCents })) }, payment: { id: payment.id, status: payment.status, redirectUrl: payment.checkout_url }, inventory: { reservationId: reservation.reservation_id, status: reservation.status } };
}

export function createServer(variant, services = defaults) {
  return async (req, res) => {
    const path = urlPath(req);
    const context = { 'x-actor-id': req.headers['x-actor-id'] || 'bff-anonymous', 'x-correlation-id': req.headers['x-correlation-id'] || req.headers['x-request-id'] || `corr_${Date.now()}` };
    try {
      if (req.method === 'OPTIONS') { res.writeHead(204, { 'access-control-allow-origin': '*', 'access-control-allow-methods': 'GET,POST,OPTIONS', 'access-control-allow-headers': 'content-type,authorization' }); return res.end(); }
      if (req.method === 'GET' && path === '/healthz') return json(res, 200, { status: 'ok', service: `${variant}-bff` });
      if (req.method === 'GET' && path === '/api/catalog') return json(res, 200, { products: catalog });
      if (req.method === 'GET' && path === '/api/audit/events') return json(res, 200, await auditEvents(req, services));
      if (req.method === 'POST' && path === '/api/checkout') return json(res, 201, await checkout(await readBody(req), variant, services, context));
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
