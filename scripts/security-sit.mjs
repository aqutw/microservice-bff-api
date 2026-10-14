import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { signInternalRequest } from '../bff-api/src/service-auth.js';

const envFile = process.env.SECURITY_ENV_FILE || '.env.security';
process.loadEnvFile(envFile);
execFileSync(process.execPath, ['scripts/test-tokens.mjs', envFile], { stdio: 'inherit' });
process.loadEnvFile('.security/tokens.env');
const composeCommand = process.env.COMPOSE_BIN || 'docker';
function compose(...args) { execFileSync(composeCommand, [...(composeCommand === 'docker' ? ['compose'] : []), ...args], { stdio: 'ignore' }); }
const web = process.env.PUBLIC_URL;
const desktop = process.env.DESKTOP_BFF_URL || `https://localhost:${process.env.DESKTOP_PORT}`;
const app = process.env.APP_BFF_URL || `https://localhost:${process.env.APP_PORT}`;
const domain = { order: `https://localhost:${process.env.ORDER_PORT}`, inventory: `https://localhost:${process.env.INVENTORY_PORT}`, payment: `https://localhost:${process.env.PAYMENT_PORT}`, 'audit-worker': `https://localhost:${process.env.AUDIT_PORT}` };
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function call(base, path, token, method = 'GET', body, headers = {}) {
 const response = await fetch(`${base}${path}`, { method, headers: { ...(token ? { authorization: `Bearer ${token}` } : {}), ...(body ? { 'content-type': 'application/json' } : {}), ...headers }, body: body && JSON.stringify(body), signal: AbortSignal.timeout(10000) });
 return { status: response.status, body: await response.json() };
}
async function internal(target, caller, path, method = 'GET', payload, actor = '') {
 const body = payload === undefined ? '' : JSON.stringify(payload);
 const config = { name: caller, keys: JSON.parse(process.env[`${caller.replaceAll('-', '_').toUpperCase()}_OUTGOING_KEYS`]) };
 return call(domain[target], path, null, method, payload, signInternalRequest(target, path, { method, body, headers: { 'x-actor-id': actor } }, config));
}
async function checkout(base, token, key) {
 let result = await call(base, '/api/checkout', token, 'POST', { user_id: 'spoofed-user', items: [{ sku: 'mug', quantity: 1 }] }, { 'idempotency-key': key, 'x-actor-id': 'spoofed-actor' });
 assert.ok([201,202].includes(result.status), `checkout: ${result.status}`);
 for (let attempt = 0; result.status === 202 && attempt < 60; attempt++) { await sleep(500); result = await call(base, `/api/checkouts/${result.body.checkoutId}`, token); }
 assert.ok([200,201].includes(result.status)); return result.body;
}
for (const url of [desktop, app, ...Object.values(domain)]) {
 for (let attempt = 0; attempt < 90; attempt++) {try { if ((await fetch(`${url}/healthz`)).ok) break; } catch {} if (attempt === 89) throw new Error('Service health timeout'); await sleep(1000);}
}
const alice = process.env.ALICE_TOKEN, bob = process.env.BOB_TOKEN, auditor = process.env.AUDITOR_TOKEN;
assert.equal((await call(desktop, '/api/checkout', null, 'POST', {})).status, 401);
const forged = `${alice.slice(0, -20)}${alice.at(-20) === 'a' ? 'b' : 'a'}${alice.slice(-19)}`;
assert.equal((await call(app, '/api/orders/unknown', forged)).status, 401);
assert.equal((await call(desktop, '/api/audit/events', alice)).status, 403);
assert.equal((await call(desktop, '/api/audit/events', auditor)).status, 200);
for (const target of Object.keys(domain)) assert.equal((await call(domain[target], target === 'audit-worker' ? '/v1/audit/events' : '/v1/unknown', alice)).status, 401);
console.log('PASS: token enforcement, audit permissions and direct-service denial');

const key = `security-${Date.now()}`;
const a = await checkout(desktop, alice, key);
const b = await checkout(desktop, bob, key);
assert.notEqual(a.checkoutId, b.checkoutId);
const replay = await checkout(desktop, alice, key);
assert.equal(replay.checkoutId, a.checkoutId);
for (const token of [bob, auditor]) for (const [path, method] of [[`/api/orders/${a.order.id}`, 'GET'], [`/api/checkouts/${a.checkoutId}`, 'GET'], [`/api/payments/${a.payment.id}/confirm`, 'POST']]) assert.equal((await call(app, path, token, method, method === 'POST' ? {} : undefined)).status, 404);
assert.equal((await call(desktop, `/api/orders/${a.order.id}`, alice)).body.user_id, process.env.ALICE_ACTOR);
assert.equal((await internal('payment', 'order', `/v1/payments/${a.payment.id}`)).body.status, 'pending');
assert.equal((await internal('payment', 'desktop-bff', `/v1/payments/${a.payment.id}/confirm`, 'POST', {}, process.env.BOB_ACTOR)).status, 404);
assert.equal((await internal('payment', 'order', `/v1/payments/${a.payment.id}/confirm`, 'POST', {})).status, 403);
assert.equal((await internal('payment', 'order', `/v1/payments/${a.payment.id}`)).body.status, 'pending');
let confirmed = await call(app, `/api/payments/${a.payment.id}/confirm`, alice, 'POST', {});
assert.ok([200,202].includes(confirmed.status));
for(let attempt=0;confirmed.status===202 && attempt<60;attempt++) { await sleep(500);confirmed=await call(app,`/api/checkouts/${confirmed.body.checkoutId}`,alice); }
assert.equal(confirmed.status,200);assert.equal(confirmed.body.state,'paid');
console.log('PASS: ownership at BFF and Payment, no rejected-payment mutation, owner-bound idempotency');

// Exercise a real browser authorization code flow with a small cookie jar.
class Browser {
 cookies = new Map();
 async request(url, options = {}) {
  const parsed = new URL(url);
  const cookie = [...this.cookies.values()].filter(item => item.host === parsed.hostname && parsed.pathname.startsWith(item.path)).map(item => `${item.name}=${item.value}`).join('; ');
  const result = await fetch(url, { ...options, redirect: 'manual', headers: { ...options.headers, cookie }, signal: AbortSignal.timeout(10000) });
  for (const line of result.headers.getSetCookie()) {
   const [pair, ...attributes] = line.split(';'); const separator = pair.indexOf('=');
   const name = pair.slice(0, separator), value = pair.slice(separator + 1);
   const path = attributes.find(item => /^\s*path=/i.test(item))?.split('=')[1] || '/';
   if (attributes.some(item => /^\s*max-age=0$/i.test(item))) this.cookies.delete(name); else this.cookies.set(name, { name, value, path, host: parsed.hostname });
  }
  return result;
 }
 async follow(result) { for (let attempt = 0; result.status >= 300 && result.status < 400 && attempt < 12; attempt++) result = await this.request(result.headers.get('location')); return result; }
}
const browser = new Browser();
let login = await browser.request(`${web}/auth/login`);
assert.equal(login.status, 302);
login = await browser.request(login.headers.get('location'));
const html = await login.text();
const action = html.match(/<form[^>]*action="([^"]+)"/i)?.[1]?.replaceAll('&amp;', '&');
assert.ok(action, 'Provider login form unavailable');
login = await browser.request(action, { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams({ username: 'alice', password: process.env.TEST_ALICE_PASSWORD, credentialId: '' }) });
const loggedIn = await browser.follow(login);
assert.equal(loggedIn.status, 200, 'OIDC callback failed');
const session = await (await browser.request(`${web}/auth/session`)).json();
assert.equal(session.authenticated, true); assert.equal(session.user.auditRead, false); assert.equal(session.accessToken, undefined);
assert.equal((await browser.request(`${web}/api/checkout`, { method: 'POST', headers: { origin: web, 'content-type': 'application/json' }, body: '{}' })).status, 403);
assert.equal((await browser.request(`${web}/api/checkout`, { method: 'POST', headers: { origin: 'https://evil.test', 'content-type': 'application/json', 'x-csrf-token': session.csrfToken }, body: '{}' })).status, 403);
const created = await browser.request(`${web}/api/checkout`, { method: 'POST', headers: { origin: web, 'content-type': 'application/json', 'x-csrf-token': session.csrfToken }, body: JSON.stringify({ items: [{ sku: 'tea-leaves', quantity: 1 }] }) });
assert.ok([201,202].includes(created.status));
compose('restart','website');
let resumed;
for(let attempt=0;attempt<60;attempt++){try{resumed=await browser.request(`${web}/auth/session`);if(resumed.ok)break;}catch{}await sleep(500);}
assert.equal((await resumed.json()).authenticated,true,'session did not survive website restart');
const retainedCookies = [...browser.cookies.values()].filter(item => item.path === '/').map(item => `${item.name}=${item.value}`).join('; ');
assert.equal((await browser.request(`${web}/auth/logout`, { method: 'POST', headers: { origin: web, 'x-csrf-token': session.csrfToken } })).status, 200);
assert.equal((await (await browser.request(`${web}/auth/session`)).json()).authenticated, false);
assert.equal((await (await fetch(`${web}/auth/session`, {headers:{cookie:retainedCookies}})).json()).authenticated,false);
console.log('PASS: real OIDC code/PKCE login, encrypted browser session, CSRF and logout');

compose('stop', 'payment');
try {
 // Accepted work contains the original owner/actor, not a persisted user token.
 const shortResponse = await fetch(`${process.env.AUTH_ISSUER}/protocol/openid-connect/token`, {method:'POST',body:new URLSearchParams({grant_type:'password',client_id:'commerce-short-test',client_secret:process.env.TEST_SHORT_CLIENT_SECRET,username:'alice',password:process.env.TEST_ALICE_PASSWORD})});
 assert.equal(shortResponse.status,200);
 const shortToken = (await shortResponse.json()).access_token;
 const shortPayload = JSON.parse(Buffer.from(shortToken.split('.')[1],'base64url'));
 assert.ok(shortPayload.exp - shortPayload.iat <= 3, 'Test token must have short expiry');
 const accepted = await call(desktop, '/api/checkout', shortToken, 'POST', { items: [{ sku: 'coffee-beans', quantity: 1 }] }, { 'idempotency-key': `${key}-recovery` });
 assert.equal(accepted.status, 202);
 await sleep(9000);
 assert.equal((await call(desktop, `/api/checkouts/${accepted.body.checkoutId}`, shortToken)).status,401);
 compose('restart', 'order'); compose('start', 'payment');
 let recovered;
 for (let attempt = 0; attempt < 90; attempt++) { await sleep(500); recovered = await call(desktop, `/api/checkouts/${accepted.body.checkoutId}`, alice); if (recovered.status === 200) break; }
 assert.equal(recovered.status, 200);
 console.log('PASS: accepted workflow resumes with service credentials after user token expiry and Order restart');
} finally { compose('start', 'payment'); }
console.log('Security SIT passed');
