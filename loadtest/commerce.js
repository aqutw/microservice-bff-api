import http from 'k6/http';
import { check, sleep } from 'k6';

const desktop = __ENV.DESKTOP_BFF_URL || 'http://127.0.0.1:3001';
const app = __ENV.APP_BFF_URL || 'http://127.0.0.1:3002';
const inventory = __ENV.INVENTORY_API_URL || 'http://127.0.0.1:8082';

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
    'http_req_duration{scenario:catalog_read}': ['p(95)<500'],
    'http_req_duration{scenario:audit_read}': ['p(95)<750'],
    'http_req_duration{scenario:desktop_checkout}': ['p(95)<1500'],
    'http_req_duration{scenario:app_checkout}': ['p(95)<1500']
  }
};

const jsonHeaders = { headers: { 'Content-Type': 'application/json', 'X-Actor-ID': 'loadtest', 'X-Correlation-ID': '' } };

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
  const response = http.get(`${desktop}/api/audit/events?limit=20`);
  check(response, { 'audit returns 200': r => r.status === 200, 'audit has events array': r => Array.isArray(body(r).events) });
}

function commitReservation(reservationID, id) {
  const response = http.post(`${inventory}/v1/reservations/${reservationID}/commit`, null, { headers: { 'X-Actor-ID': 'loadtest', 'X-Correlation-ID': id } });
  return check(response, { 'inventory commit returns 200': r => r.status === 200 && body(r).status === 'committed' });
}

export function desktopCheckout() {
  const id = correlation('desktop');
  const options = { headers: { ...jsonHeaders.headers, 'X-Correlation-ID': id } };
  const checkout = http.post(`${desktop}/api/checkout`, JSON.stringify({ user_id: `loadtest-desktop-${__VU}-${__ITER}`, items: [{ sku: 'mug', quantity: 1, unit_price_cents: 650 }] }), options);
  const checkoutBody = body(checkout);
  const checkoutOK = check(checkout, { 'desktop checkout returns 201': r => r.status === 201, 'desktop returns payment and reservation': r => Boolean(checkoutBody.payment?.id && checkoutBody.inventory?.reservationId) });
  if (!checkoutOK) return;
  const payment = http.post(`${desktop}/api/payments/${checkoutBody.payment.id}/confirm`, '{}', options);
  const paymentOK = check(payment, { 'desktop payment returns paid': r => r.status === 200 && body(r).payment?.status === 'paid' });
  if (paymentOK) commitReservation(checkoutBody.inventory.reservationId, id);
  sleep(0.1);
}

export function appCheckout() {
  const id = correlation('app');
  const options = { headers: { ...jsonHeaders.headers, 'X-Correlation-ID': id } };
  const checkout = http.post(`${app}/api/checkout`, JSON.stringify({ user_id: `loadtest-app-${__VU}-${__ITER}`, items: [{ sku: 'tea-leaves', quantity: 1, unit_price_cents: 280 }] }), options);
  const checkoutBody = body(checkout);
  const checkoutOK = check(checkout, { 'app checkout returns 201': r => r.status === 201, 'app returns client token': r => Boolean(checkoutBody.payment?.id && checkoutBody.payment?.clientToken) });
  if (!checkoutOK) return;
  const payment = http.post(`${app}/api/payments/${checkoutBody.payment.id}/confirm`, '{}', options);
  const paymentOK = check(payment, { 'app payment returns paid': r => r.status === 200 && body(r).state === 'paid' });
  if (paymentOK) commitReservation(checkoutBody.reservationId, id);
  sleep(0.1);
}
