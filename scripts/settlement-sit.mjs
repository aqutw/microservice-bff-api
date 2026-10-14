// Runs real domain binaries against isolated databases and generated test credentials.
// Identity is a BFF fixture; real OIDC is covered separately by security-sit.mjs.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, appendFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseEnv } from 'node:util';
import { once } from 'node:events';
import { signInternalRequest } from '../bff-api/src/service-auth.js';
import { createServer } from '../bff-api/src/server.js';

const repository = resolve(fileURLToPath(new URL('..', import.meta.url)));
const services = ['audit-worker', 'inventory', 'payment', 'order'];
if (process.argv[2] !== '--run') {
 const workspace = mkdtempSync(join(tmpdir(), 'commerce-settlement-'));
 try {
  execFileSync(process.execPath, [join(repository, 'scripts/init-security.mjs')], { cwd: workspace, stdio: 'ignore' });
  for (const service of services) execFileSync('go', ['build', '-o', join(workspace, service), '.'], { cwd: join(repository, 'services', service), stdio: 'inherit' });
  execFileSync(process.execPath, [fileURLToPath(import.meta.url), '--run', workspace], {
   cwd: repository, stdio: 'inherit', env: { ...process.env, NODE_EXTRA_CA_CERTS: join(workspace, '.security/pki/ca.pem') }
  });
  rmSync(workspace, { recursive: true, force: true });
 } catch (error) {
  console.error(`Settlement SIT failed; isolated diagnostics retained at ${workspace}`);
  process.exitCode = 1;
 }
} else {
 await run(process.argv[3]);
}

async function run(workspace) {
 Object.assign(process.env, parseEnv(readFileSync(join(workspace, '.env.security'), 'utf8')));
 const ca = readFileSync(join(workspace, '.security/pki/ca.pem'));
 const certificate = peer => ({ key: readFileSync(join(workspace, `.security/pki/${peer}.key`)), cert: readFileSync(join(workspace, `.security/pki/${peer}.pem`)) });
 const actor = 'usr_settlement_test';
 const ports = {}, urls = {}, processes = new Map(), servers = [];
 const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
 const prefix = peer => peer.replaceAll('-', '_').toUpperCase();
 const faults = { commit: '', refund: '' };

 async function freePort() {
  const listener = net.createServer(); listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
  const port = listener.address().port; await new Promise(resolve => listener.close(resolve)); return port;
 }
 async function waitFor(check, message) {
  for (let attempt = 0; attempt < 150; attempt++) { try { if (await check()) return; } catch {} await sleep(100); }
  throw new Error(message);
 }
 function sql(service, statement, values = []) {
  const script = `import sqlite3,json,sys\nc=sqlite3.connect(sys.argv[1],timeout=5)\nr=c.execute(sys.argv[2],json.loads(sys.argv[3]))\nprint(json.dumps(r.fetchall()))\nc.commit()`;
  return JSON.parse(execFileSync('python3', ['-c', script, join(workspace, `${service}.db`), statement, JSON.stringify(values)], { encoding: 'utf8' }));
 }
 async function internal(target, caller, path, method = 'GET', payload, owner = actor) {
  const body = payload === undefined ? '' : JSON.stringify(payload);
  const headers = signInternalRequest(target, path, { method, body, headers: { 'x-actor-id': owner, 'x-correlation-id': 'settlement-sit' } }, { name: caller, keys: JSON.parse(process.env[`${prefix(caller)}_OUTGOING_KEYS`]) });
  const result = await fetch(urls[target] + path, { method, headers: { ...headers, 'content-type': 'application/json' }, body: method === 'GET' ? undefined : body, signal: AbortSignal.timeout(10000) });
  return { status: result.status, body: await result.json() };
 }
 async function start(service) {
  const env = {
   ...process.env, SERVICE_NAME: service,
   SERVICE_INCOMING_KEYS: process.env[`${prefix(service)}_INCOMING_KEYS`], SERVICE_OUTGOING_KEYS: process.env[`${prefix(service)}_OUTGOING_KEYS`],
   SERVICE_TLS_CERT: join(workspace, `.security/pki/${service}.pem`), SERVICE_TLS_KEY: join(workspace, `.security/pki/${service}.key`), SERVICE_CA_FILE: join(workspace, '.security/pki/ca.pem'),
   PORT: String(ports[service]), DB_PATH: join(workspace, `${service}.db`), RESERVATION_TTL_SECONDS: '900',
   INVENTORY_API_URL: urls['inventory-proxy'], PAYMENT_API_URL: urls['payment-proxy'], ORDER_API_URL: urls.order, AUDIT_WORKER_URL: urls['audit-worker']
  };
  const child = spawn(join(workspace, service), [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  processes.set(service, child);
  for (const stream of [child.stdout, child.stderr]) stream.on('data', data => appendFileSync(join(workspace, `${service}.log`), data));
  await waitFor(async () => (await fetch(urls[service] + '/healthz', { signal: AbortSignal.timeout(1000) })).ok, `${service} did not start`);
 }
 async function stop(service) {
  const child = processes.get(service);
  if (!child) return;
  processes.delete(service);
  if (child.exitCode === null && child.signalCode === null) { const exited = once(child, 'exit'); child.kill('SIGTERM'); await exited; }
 }
 async function proxy(target) {
  const server = https.createServer(certificate(target), (req, res) => {
   const fault = req.url.endsWith('/commit') ? 'commit' : req.url.endsWith('/refund') ? 'refund' : null;
   // Keep refund lookup unavailable after its lost response until the restart test restores it.
   if ((fault && faults[fault] === 'fail') || (target === 'payment' && faults.refund === 'fail' && req.method === 'GET')) {
    res.writeHead(503, { 'content-type': 'application/json' }); res.end('{"error":"injected_outage"}'); return;
   }
   const upstream = https.request(urls[target] + req.url, { method: req.method, headers: req.headers, ca }, response => {
    if (fault && faults[fault] === 'drop' && response.statusCode === 200) {
     faults[fault] = 'fail'; response.resume(); response.on('end', () => res.destroy()); return;
    }
    res.writeHead(response.statusCode, response.headers); response.pipe(res);
   });
   upstream.on('error', () => { if (!res.headersSent) res.writeHead(503, { 'content-type': 'application/json' }); res.end('{"error":"proxy_unavailable"}'); });
   req.pipe(upstream);
  });
  server.listen(ports[`${target}-proxy`], '127.0.0.1'); await once(server, 'listening'); servers.push(server);
 }
 let bff;
 async function api(path, method = 'GET', body) {
  const response = await fetch(bff + path, { method, headers: { 'content-type': 'application/json', 'idempotency-key': path }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(10000) });
  return { status: response.status, body: await response.json() };
 }
 async function checkout(key) {
  // Each workflow needs a distinct idempotency key, independent of its URL.
  const response = await fetch(bff + '/api/checkout', { method: 'POST', headers: { 'content-type': 'application/json', 'idempotency-key': key }, body: JSON.stringify({ items: [{ sku: 'mug', quantity: 1 }] }) });
  let result = { status: response.status, body: await response.json() };
  const id = result.body.checkoutId;
  await waitFor(async () => { if (result.status === 202) result = await api(`/api/checkouts/${id}`); return result.status === 200 || result.status === 201; }, 'checkout did not become ready');
  return result.body;
 }
 async function paid(checkout) {
  await waitFor(async () => (await api(`/api/checkouts/${checkout.checkoutId}`)).body.state === 'paid', 'settlement did not finish');
  const order = await api(`/api/orders/${checkout.order.id}`);
  const hold = await internal('inventory', 'order', `/v1/reservations/${checkout.inventory.reservationId}`);
  const payment = await internal('payment', 'order', `/v1/payments/${checkout.payment.id}`);
  assert.equal(order.body.status, 'paid'); assert.equal(hold.body.status, 'committed'); assert.equal(payment.body.status, 'paid');
 }
 async function cancelled(checkout, paymentStatus) {
  await waitFor(async () => (await api(`/api/orders/${checkout.order.id}`)).body.status === 'cancelled', 'compensation did not finish');
  assert.equal((await api(`/api/checkouts/${checkout.checkoutId}`)).status, 409);
  assert.equal((await internal('inventory', 'order', `/v1/reservations/${checkout.inventory.reservationId}`)).body.status, 'released');
  assert.equal((await internal('payment', 'order', `/v1/payments/${checkout.payment.id}`)).body.status, paymentStatus);
 }
 async function idleStage(checkout, stage) {
  await waitFor(() => sql('order', 'SELECT stage,lease_until,attempts FROM checkouts WHERE id=?', [checkout.checkoutId]).some(([s, lease, attempts]) => s === stage && lease === 0 && attempts > 0), `workflow did not persist ${stage}`);
 }

 try {
  for (const service of [...services, 'inventory-proxy', 'payment-proxy']) { ports[service] = await freePort(); urls[service] = `https://127.0.0.1:${ports[service]}`; }
  await proxy('inventory'); await proxy('payment');
  for (const service of services) await start(service);
  process.env.SERVICE_NAME = 'desktop-bff'; process.env.SERVICE_OUTGOING_KEYS = process.env.DESKTOP_BFF_OUTGOING_KEYS;
  const server = http.createServer(createServer('desktop', { order: urls.order, payment: urls.payment, inventory: urls.inventory, audit: urls['audit-worker'] }, { authenticate: async () => ({ id: actor, auditRead: true }) }));
  server.listen(0, '127.0.0.1'); await once(server, 'listening'); servers.push(server); bff = `http://127.0.0.1:${server.address().port}`;

  const normal = await checkout('normal');
  const confirmed = await api(`/api/payments/${normal.payment.id}/confirm`, 'POST', {});
  assert.ok([200, 202].includes(confirmed.status)); await paid(normal);
  assert.equal((await internal('order', 'desktop-bff', `/v1/orders/${normal.order.id}`, 'POST', { status: 'paid' })).status, 409);
  assert.equal((await internal('order', 'desktop-bff', `/v1/orders/${normal.order.id}/settlement`, 'POST', {}, 'usr_other')).status, 404);
  assert.equal((await internal('payment', 'desktop-bff', `/v1/payments/${normal.payment.id}/refund`, 'POST', {})).status, 403);
  console.log('PASS: real BFF settlement, committed inventory, ownership and compensation permissions');

  const pending = await checkout('pending-cancel');
  assert.ok([200, 202].includes((await api(`/api/orders/${pending.order.id}/cancel`, 'POST', {})).status));
  await cancelled(pending, 'cancelled');
  assert.equal((await api(`/api/payments/${pending.payment.id}/confirm`, 'POST', {})).status, 409);
  console.log('PASS: cancellation closes Payment and Inventory and checkout returns HTTP 409');

  const lost = await checkout('lost-commit'); faults.commit = 'drop';
  assert.equal((await api(`/api/payments/${lost.payment.id}/confirm`, 'POST', {})).status, 202);
  await idleStage(lost, 'settling');
  assert.equal((await api(`/api/orders/${lost.order.id}`)).body.status, 'settling');
  await stop('order'); faults.commit = ''; await start('order'); await paid(lost);
  assert.equal(sql('inventory', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='inventory.committed'", [lost.inventory.reservationId])[0][0], 1);
  console.log('PASS: lost commit response and Order restart recover without double stock/audit changes');

  const expired = await checkout('expired-refund');
  sql('inventory', 'UPDATE reservations SET expires_at=? WHERE id=?', [Date.now() - 1000, expired.inventory.reservationId]);
  faults.refund = 'drop';
  assert.equal((await api(`/api/payments/${expired.payment.id}/confirm`, 'POST', {})).status, 202);
  await idleStage(expired, 'refund_pending');
  assert.equal((await api(`/api/orders/${expired.order.id}`)).body.status, 'refund_pending');
  await stop('order'); faults.refund = ''; await start('order'); await cancelled(expired, 'refunded');
  assert.equal(sql('payment', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='payment.refunded'", [expired.payment.id])[0][0], 1);
  console.log('PASS: expired inventory and lost refund response recover a single refund after restart');

  const racing = await checkout('settlement-cancel-race'); faults.commit = 'fail';
  assert.equal((await api(`/api/payments/${racing.payment.id}/confirm`, 'POST', {})).status, 202);
  await idleStage(racing, 'settling');
  assert.equal((await api(`/api/orders/${racing.order.id}/cancel`, 'POST', {})).status, 409);
  faults.commit = ''; await paid(racing);
  console.log('PASS: settling order rejects a cancellation that would release sold inventory');

  const recovery = await checkout('paid-without-trigger');
  sql('order', 'UPDATE checkouts SET next_attempt=? WHERE id=?', [Date.now() + 3000, recovery.checkoutId]);
  assert.equal((await internal('payment', 'desktop-bff', `/v1/payments/${recovery.payment.id}/confirm`, 'POST', {})).status, 200);
  await stop('order'); await start('order'); await paid(recovery);
  console.log('PASS: durable payment reconciliation needs no BFF retry');

  const legacy = await checkout('legacy-paid');
  sql('order', 'UPDATE checkouts SET next_attempt=? WHERE id=?', [Date.now() + 300000, legacy.checkoutId]);
  assert.equal((await internal('payment', 'desktop-bff', `/v1/payments/${legacy.payment.id}/confirm`, 'POST', {})).status, 200);
  await stop('order');
  // Reproduce the previous BFF publishing paid before inventory was committed.
  sql('order', "UPDATE orders SET status='paid' WHERE id=?", [legacy.order.id]);
  await start('order'); await paid(legacy);
  assert.equal(sql('order', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='order.settlement_recovering'", [legacy.order.id])[0][0], 1);
  assert.equal((await internal('inventory', 'order', '/v1/stock/mug')).body.available, 15);
  console.log('PASS: previous-version paid workflow is recovered on upgrade; stock matches five sales');
  console.log('Settlement SIT passed');
 } finally {
  for (const service of services.toReversed()) await stop(service);
  for (const server of servers) { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
 }
}
