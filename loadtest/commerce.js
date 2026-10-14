import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate } from 'k6/metrics';

const desktopCompleted = new Rate('desktop_transactions_completed');
const appCompleted = new Rate('app_transactions_completed');
const desktopAttempted = new Counter('desktop_transactions_attempted');
const appAttempted = new Counter('app_transactions_attempted');

const desktop = __ENV.DESKTOP_BFF_URL || 'https://127.0.0.1:3001';
const app = __ENV.APP_BFF_URL || 'https://127.0.0.1:3002';

export const options = {
  scenarios: {
    catalog_read: {
      executor: 'constant-arrival-rate',
      rate: 20,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      maxVUs: 30,
      exec: 'catalogRead'
    },
    audit_read: {
      executor: 'constant-arrival-rate',
      rate: 10,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 5,
      maxVUs: 20,
      startTime: '2s',
      exec: 'auditRead'
    },
    desktop_checkout: {
      executor: 'per-vu-iterations',
      vus: 5,
      iterations: 3,
      maxDuration: '45s',
      startTime: '2s',
      exec: 'desktopCheckout'
    },
    app_checkout: {
      executor: 'per-vu-iterations',
      vus: 3,
      iterations: 2,
      maxDuration: '45s',
      startTime: '2s',
      exec: 'appCheckout'
    }
  },
  thresholds: {
    checks: ['rate>0.95'],
    http_req_failed: ['rate<0.05'],
    'checks{scenario:catalog_read}': ['rate>0.95'],
    'checks{scenario:audit_read}': ['rate>0.95'],
    'checks{scenario:desktop_checkout}': ['rate==1'],
    'checks{scenario:app_checkout}': ['rate==1'],
    'http_req_failed{scenario:desktop_checkout}': ['rate==0'],
    'http_req_failed{scenario:app_checkout}': ['rate==0'],
    desktop_transactions_completed: ['rate==1'],
    app_transactions_completed: ['rate==1'],
    desktop_transactions_attempted: ['count==15'],
    app_transactions_attempted: ['count==6'],
    'http_req_duration{scenario:catalog_read}': ['p(95)<500'],
    'http_req_duration{scenario:audit_read}': ['p(95)<750'],
    'http_req_duration{scenario:desktop_checkout}': ['p(95)<1500'],
    'http_req_duration{scenario:app_checkout}': ['p(95)<1500']
  }
};

const jsonHeaders = { headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${__ENV.ACCESS_TOKEN || ''}`, 'X-Correlation-ID': '' } };

function body(response) {
  try { return response.json(); } catch (_) { return {}; }
}

function correlation(prefix) {
  return `loadtest-${prefix}-${__VU}-${__ITER}`;
}

export function catalogRead() {
  const response = http.get(`${desktop}/api/catalog`);
  check(response, { 'catalog returns 200': r => r.status === 200, 'catalog has products': r => Array.isArray(body(r).products) });
}

export function auditRead() {
  const response = http.get(`${desktop}/api/audit/events?limit=20`, jsonHeaders);
  check(response, { 'audit returns 200': r => r.status === 200, 'audit has events array': r => Array.isArray(body(r).events) });
}

function resolveCheckout(response, base) {
  if (response.status !== 202) return response;
  const checkoutId = body(response).checkoutId;
  if (!checkoutId) return response;
  for (let attempt = 0; attempt < 30; attempt++) {
    sleep(0.1);
    response = http.get(`${base}/api/checkouts/${encodeURIComponent(checkoutId)}`, jsonHeaders);
    if (response.status !== 202) return response;
  }
  return response;
}

function runDesktopCheckout() {
  let completed = false;
  const id = correlation('desktop');
  const options = { headers: { ...jsonHeaders.headers, 'X-Correlation-ID': id } };
  const checkout = resolveCheckout(http.post(`${desktop}/api/checkout`, JSON.stringify({ user_id: `loadtest-desktop-${__VU}-${__ITER}`, items: [{ sku: 'mug', quantity: 1, unit_price_cents: 650 }] }), options), desktop);
  const checkoutBody = body(checkout);
  const checkoutOK = check(checkout, { 'desktop checkout is ready': r => r.status === 201 || r.status === 200, 'desktop returns payment and reservation': r => Boolean(checkoutBody.payment?.id && checkoutBody.inventory?.reservationId) });
  if (!checkoutOK) return false;
  const payment = resolveCheckout(http.post(`${desktop}/api/payments/${checkoutBody.payment.id}/confirm`, '{}', options), desktop);
  const paymentOK = check(payment, { 'desktop payment returns paid': r => r.status === 200 && body(r).state === 'paid' && body(r).payment?.status === 'paid' && (body(r).orderStatus === 'paid' || body(r).order?.status === 'paid') });
  completed = paymentOK;
  sleep(0.1);
  return completed;
}

function runAppCheckout() {
  let completed = false;
  const id = correlation('app');
  const options = { headers: { ...jsonHeaders.headers, 'X-Correlation-ID': id } };
  const checkout = resolveCheckout(http.post(`${app}/api/checkout`, JSON.stringify({ user_id: `loadtest-app-${__VU}-${__ITER}`, items: [{ sku: 'tea-leaves', quantity: 1, unit_price_cents: 280 }] }), options), app);
  const checkoutBody = body(checkout);
  const checkoutOK = check(checkout, { 'app checkout is ready': r => r.status === 201 || r.status === 200, 'app returns client token': r => Boolean(checkoutBody.payment?.id && checkoutBody.payment?.clientToken) });
  if (!checkoutOK) return false;
  const payment = resolveCheckout(http.post(`${app}/api/payments/${checkoutBody.payment.id}/confirm`, '{}', options), app);
  const paymentOK = check(payment, { 'app payment returns paid': r => r.status === 200 && body(r).state === 'paid' });
  completed = paymentOK;
  sleep(0.1);
  return completed;
}

// Keep metric recording outside the transaction function. Older k6 JS engines
// mishandle lexical bindings in finally blocks when checkout returns early.
export function desktopCheckout() {
  desktopAttempted.add(1);
  try { desktopCompleted.add(runDesktopCheckout()); }
  catch (error) { desktopCompleted.add(false); throw error; }
}

export function appCheckout() {
  appAttempted.add(1);
  try { appCompleted.add(runAppCheckout()); }
  catch (error) { appCompleted.add(false); throw error; }
}
