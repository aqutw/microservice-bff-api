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
import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
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
 const faults = { reserve: '', create_payment: '', commit: '', refund: '', cancel: '', release: '' };
 let heldCommit;

 async function freePort() {
  const listener = net.createServer(); listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
  const port = listener.address().port; await new Promise(resolve => listener.close(resolve)); return port;
 }
 async function waitFor(check, message, timeout = 15000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) { try { if (await check()) return; } catch {} await sleep(100); }
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
   INVENTORY_API_URL: urls['inventory-proxy'], PAYMENT_API_URL: urls['payment-proxy'], ORDER_API_URL: urls['order-proxy'], AUDIT_WORKER_URL: urls['audit-worker']
  };
  const child = spawn(join(workspace, service), [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  processes.set(service, child);
  for (const stream of [child.stdout, child.stderr]) stream.on('data', data => appendFileSync(join(workspace, `${service}.log`), data));
  await waitFor(async () => (await fetch(urls[service] + '/healthz', { signal: AbortSignal.timeout(1000) })).ok, `${service} did not start`);
 }
 async function stop(service, signal = 'SIGTERM') {
  const child = processes.get(service);
  if (!child) return;
  processes.delete(service);
  if (child.exitCode === null && child.signalCode === null) { const exited = once(child, 'exit'); if (signal === 'SIGTERM') child.kill('SIGCONT'); child.kill(signal); await exited; }
 }
 async function proxy(target) {
  const server = https.createServer(certificate(target), (req, res) => {
   const fault = req.method !== 'POST' ? null : req.url === '/v1/reservations' ? 'reserve' : req.url === '/v1/payments' ? 'create_payment' :
    ['commit', 'refund', 'cancel', 'release'].find(action => req.url.endsWith(`/${action}`));
   // Hold reconciliation until each lost-response assertion has observed the durable intermediate state.
   if ((fault && ['fail', 'outage'].includes(faults[fault])) || (target === 'payment' && ['refund', 'cancel', 'create_payment'].some(action => faults[action] === 'fail') && req.method === 'GET')) {
    res.writeHead(503, { 'content-type': 'application/json' }); res.end('{"error":"injected_outage"}'); return;
   }
   const upstream = https.request(urls[target] + req.url, { method: req.method, headers: req.headers, ca }, response => {
    if (fault === 'commit' && faults.commit === 'hold' && response.statusCode === 200) {
     response.resume(); response.on('end', () => { heldCommit = { reservationId: req.url.split('/')[3], response: res }; }); return;
    }
    if (fault && faults[fault] === 'drop' && [200, 201].includes(response.statusCode)) {
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
  for (const service of [...services, 'inventory-proxy', 'payment-proxy', 'order-proxy', 'app-bff', 'jwks']) { ports[service] = await freePort(); urls[service] = `https://127.0.0.1:${ports[service]}`; }
  await proxy('inventory'); await proxy('payment'); await proxy('order');
  for (const service of services) await start(service);
  process.env.SERVICE_NAME = 'desktop-bff'; process.env.SERVICE_OUTGOING_KEYS = process.env.DESKTOP_BFF_OUTGOING_KEYS;
  const domains = { order: urls['order-proxy'], payment: urls.payment, inventory: urls.inventory, audit: urls['audit-worker'] };
  const server = http.createServer(createServer('desktop', domains, { authenticate: async () => ({ id: actor, auditRead: true }) }));
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

  for (const [action, stage] of [['reserve', 'reserve'], ['create_payment', 'payment']]) {
   const before = (await internal('inventory', 'order', '/v1/stock/mug')).body.available;
   faults[action] = 'drop';
   const response = await fetch(bff + '/api/checkout', { method: 'POST', headers: { 'content-type': 'application/json', 'idempotency-key': `lost-${action}` }, body: JSON.stringify({ items: [{ sku: 'mug', quantity: 1 }] }) });
   assert.equal(response.status, 202);
   const accepted = await response.json();
   const orderID = sql('order', 'SELECT order_id FROM checkouts WHERE id=?', [accepted.checkoutId])[0][0];
   await idleStage(accepted, stage);
   assert.equal(sql('inventory', 'SELECT count(*) FROM reservations WHERE order_id=?', [orderID])[0][0], 1);
   assert.equal(sql('payment', 'SELECT count(*) FROM payments WHERE order_id=?', [orderID])[0][0], action === 'reserve' ? 0 : 1);
   assert.equal((await api(`/api/orders/${orderID}/cancel`, 'POST', {})).status, 202);
   assert.deepEqual(sql('order', 'SELECT stage,cancel_requested FROM checkouts WHERE id=?', [accepted.checkoutId]), [[stage, 1]]);
   faults[action] = '';
   await waitFor(async () => (await api(`/api/orders/${orderID}`)).body.status === 'cancelled', `lost ${action} cancellation incomplete`);
   assert.equal((await api(`/api/checkouts/${accepted.checkoutId}`)).status, 409);
   assert.deepEqual(sql('inventory', 'SELECT status FROM reservations WHERE order_id=?', [orderID]), [['released']]);
   assert.deepEqual(sql('payment', 'SELECT status FROM payments WHERE order_id=?', [orderID]), action === 'reserve' ? [] : [['cancelled']]);
   assert.equal((await internal('inventory', 'order', '/v1/stock/mug')).body.available, before);
   console.log(`PASS: lost ${action} response recovers one resource before cancellation and restores stock`);
  }
  for (const action of ['cancel', 'release']) {
   const before = (await internal('inventory', 'order', '/v1/stock/mug')).body.available;
   const lostCompensation = await checkout(`lost-${action}`);
   faults[action] = 'drop';
   assert.equal((await api(`/api/orders/${lostCompensation.order.id}/cancel`, 'POST', {})).status, 202);
   await idleStage(lostCompensation, 'compensate');
   const service = action === 'cancel' ? 'payment' : 'inventory';
   const aggregate = action === 'cancel' ? lostCompensation.payment.id : lostCompensation.inventory.reservationId;
   const event = action === 'cancel' ? 'payment.cancelled' : 'inventory.released';
   assert.equal(sql(service, 'SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type=?', [aggregate, event])[0][0], 1);
   assert.equal((await api(`/api/orders/${lostCompensation.order.id}`)).body.status, 'cancelling');
   faults[action] = '';
   await cancelled(lostCompensation, 'cancelled');
   assert.equal(sql(service, 'SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type=?', [aggregate, event])[0][0], 1);
   // before includes no hold; checkout reserved one unit and cancellation must restore exactly one.
   assert.equal((await internal('inventory', 'order', '/v1/stock/mug')).body.available, before);
   console.log(`PASS: lost ${action} response replays compensation without duplicate audit or stock changes`);
  }

  for (const interruption of ['crash', 'stale-worker']) {
   const inFlight = await checkout(`in-flight-${interruption}`);
   heldCommit = undefined; faults.commit = 'hold';
   // Attach the rejection handler immediately: the BFF can time out while recovery waits for the lease.
   const confirmation = api(`/api/payments/${inFlight.payment.id}/confirm`, 'POST', {}).then(result => ({ result }), error => ({ error }));
   await waitFor(() => heldCommit?.reservationId === inFlight.inventory.reservationId, 'commit did not reach the response barrier');
   const [[stage, lease]] = sql('order', 'SELECT stage,lease_until FROM checkouts WHERE id=?', [inFlight.checkoutId]);
   assert.equal(stage, 'settling'); assert.ok(lease > Date.now() + 50000, 'expected a live production 60-second lease');
   assert.equal((await internal('inventory', 'order', `/v1/reservations/${inFlight.inventory.reservationId}`)).body.status, 'committed');
   if (interruption === 'crash') {
    await stop('order', 'SIGKILL');
   } else {
    const oldWorker = processes.get('order');
    oldWorker.kill('SIGSTOP'); processes.set('order-stale', oldWorker);
    ports.order = await freePort(); urls.order = `https://127.0.0.1:${ports.order}`;
   }
   heldCommit.response.destroy(); faults.commit = '';
   await start('order');
   await sleep(1000);
   const [[beforeStage, beforeLease]] = sql('order', 'SELECT stage,lease_until FROM checkouts WHERE id=?', [inFlight.checkoutId]);
   assert.equal(beforeStage, 'settling'); assert.equal(beforeLease, lease);
   console.log(`CHECK: ${interruption} preserved the active lease; waiting for its actual expiry`);
   await waitFor(() => sql('order', 'SELECT stage FROM checkouts WHERE id=?', [inFlight.checkoutId])[0][0] === 'paid', 'expired lease was not reclaimed', Math.max(0, lease - Date.now()) + 15000);
   await paid(inFlight);
   if (interruption === 'stale-worker') {
    processes.get('order-stale').kill('SIGCONT');
    await waitFor(() => readFileSync(join(workspace, 'order.log'), 'utf8').includes(`checkout ${inFlight.checkoutId} stage settling will retry`), 'stale worker did not resume its old claim');
    await stop('order-stale');
   }
   const completed = await confirmation;
   assert.ok(completed.error || completed.result.status >= 500, 'interrupted confirmation must report an unknown outcome');
   assert.deepEqual(sql('order', 'SELECT stage,lease_until,attempts FROM checkouts WHERE id=?', [inFlight.checkoutId]), [['paid', 0, 0]]);
   assert.equal(sql('order', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='order.status_changed'", [inFlight.order.id])[0][0], 1);
   assert.equal(sql('inventory', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='inventory.committed'", [inFlight.inventory.reservationId])[0][0], 1);
   console.log(`PASS: ${interruption} during RPC recovers after real lease expiry without stale writes or duplicate sale/audit`);
  }

  const beforeRestart = (await internal('inventory', 'order', '/v1/stock/mug')).body.available;
  const inventoryRestart = await checkout('inventory-process-restart');
  await stop('inventory');
  assert.equal((await api(`/api/payments/${inventoryRestart.payment.id}/confirm`, 'POST', {})).status, 202);
  await idleStage(inventoryRestart, 'settling');
  assert.deepEqual(sql('inventory', 'SELECT status FROM reservations WHERE id=?', [inventoryRestart.inventory.reservationId]), [['reserved']]);
  await start('inventory'); await paid(inventoryRestart);
  assert.equal((await internal('inventory', 'order', '/v1/stock/mug')).body.available, beforeRestart - 1);
  assert.equal(sql('inventory', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='inventory.committed'", [inventoryRestart.inventory.reservationId])[0][0], 1);
  console.log('PASS: Inventory process restart preserves the hold and completes one sale and audit event');

  const beforeRefundRestart = (await internal('inventory', 'order', '/v1/stock/mug')).body.available;
  const paymentRestart = await checkout('payment-process-restart');
  sql('inventory', 'UPDATE reservations SET expires_at=? WHERE id=?', [Date.now() - 1000, paymentRestart.inventory.reservationId]);
  // Block only the refund mutation first, so Order can still establish that Payment is paid.
  faults.refund = 'outage';
  assert.equal((await api(`/api/payments/${paymentRestart.payment.id}/confirm`, 'POST', {})).status, 202);
  await idleStage(paymentRestart, 'refund_pending');
  const attempts = sql('order', 'SELECT attempts FROM checkouts WHERE id=?', [paymentRestart.checkoutId])[0][0];
  await stop('payment'); faults.refund = '';
  await waitFor(() => sql('order', 'SELECT stage,attempts FROM checkouts WHERE id=?', [paymentRestart.checkoutId]).some(([stage, count]) => stage === 'refund_pending' && count > attempts), 'refund did not remain pending while Payment was stopped');
  assert.deepEqual(sql('payment', 'SELECT status FROM payments WHERE id=?', [paymentRestart.payment.id]), [['paid']]);
  await start('payment'); await cancelled(paymentRestart, 'refunded');
  assert.equal((await internal('inventory', 'order', '/v1/stock/mug')).body.available, beforeRefundRestart);
  assert.equal(sql('payment', "SELECT count(*) FROM audit_outbox WHERE aggregate_id=? AND event_type='payment.refunded'", [paymentRestart.payment.id])[0][0], 1);
  console.log('PASS: Payment process restart preserves refund_pending and completes one refund without stock drift');

  // Exercise the actual App process and its separate service credentials. Domain faults remain shared above.
  const jose=await import(createRequire(new URL('../bff-api/package.json',import.meta.url)).resolve('jose'));
  const {publicKey,privateKey}=await jose.generateKeyPair('RS256');
  const jwk={...await jose.exportJWK(publicKey),kid:'app-sit',alg:'RS256',use:'sig'};
  const issuer=`http://127.0.0.1:${ports.jwks}/issuer`;
  const jwks=http.createServer((req,res)=>{res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({keys:[jwk]}));});
  jwks.listen(ports.jwks,'127.0.0.1');await once(jwks,'listening');servers.push(jwks);
  const token=await new jose.SignJWT({scope:'audit:read'}).setProtectedHeader({alg:'RS256',kid:jwk.kid}).setIssuer(issuer).setAudience('commerce-api').setSubject('app-owner').setIssuedAt().setExpirationTime('5m').sign(privateKey);
  const appActor=`usr_${createHash('sha256').update(`${issuer}\0app-owner`).digest('hex')}`;
  const appProcess=spawn(process.execPath,[join(repository,'bff-api/src/app.js')],{env:{...process.env,
   SERVICE_NAME:'app-bff',SERVICE_OUTGOING_KEYS:process.env.APP_BFF_OUTGOING_KEYS,
   SERVICE_TLS_CERT:join(workspace,'.security/pki/app-bff.pem'),SERVICE_TLS_KEY:join(workspace,'.security/pki/app-bff.key'),SERVICE_CA_FILE:join(workspace,'.security/pki/ca.pem'),
   PORT:String(ports['app-bff']),ORDER_API_URL:urls['order-proxy'],PAYMENT_API_URL:urls.payment,
   AUTH_ISSUER:issuer,AUTH_JWKS_URL:`http://127.0.0.1:${ports.jwks}/jwks`,AUTH_AUDIENCE:'commerce-api',DEPLOYMENT_ENV:'development',AUTH_ALLOW_HTTP:'true'
  },stdio:['ignore','pipe','pipe']});
  processes.set('app-bff',appProcess);
  for(const stream of [appProcess.stdout,appProcess.stderr])stream.on('data',data=>appendFileSync(join(workspace,'app-bff.log'),data));
  await waitFor(async()=> (await fetch(urls['app-bff']+'/healthz')).ok,'App BFF did not start');
  const appApi=async(path,method='GET',body)=>{
   const result=await fetch(urls['app-bff']+path,{method,headers:{authorization:`Bearer ${token}`,'content-type':'application/json','idempotency-key':path},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(10000)});
   return {status:result.status,body:await result.json()};
  };
  assert.equal((await fetch(urls['app-bff']+'/api/orders/unknown')).status,401);
  const appCheckout=async key=>{
   const response=await fetch(urls['app-bff']+'/api/checkout',{method:'POST',headers:{authorization:`Bearer ${token}`,'content-type':'application/json','idempotency-key':`app-${key}`},body:JSON.stringify({items:[{sku:'mug',quantity:1}]})});
   let result={status:response.status,body:await response.json()};assert.ok([201,202].includes(result.status));
   const id=result.body.checkoutId;
   await waitFor(async()=>{if(result.status===202)result=await appApi(`/api/checkouts/${id}`);return [200,201].includes(result.status);},'App checkout not ready');
   assert.equal(typeof result.body.payment.clientToken,'string');assert.ok(result.body.payment.clientToken);assert.equal(result.body.payment.redirectUrl,undefined);
   assert.equal((await appApi(`/api/orders/${result.body.orderId}`)).body.user_id,appActor);
   return result.body;
  };
  const appPaid=await appCheckout('paid');faults.commit='fail';
  const appAccepted=await appApi(`/api/payments/${appPaid.payment.id}/confirm`,'POST',{});
  assert.equal(appAccepted.status,202);assert.equal(appAccepted.body.state,'settling');assert.equal(appAccepted.body.orderId,appPaid.orderId);assert.equal(appAccepted.body.payment,undefined);
  faults.commit='';let appFinished;
  await waitFor(async()=>{appFinished=await appApi(`/api/checkouts/${appPaid.checkoutId}`);return appFinished.body.state==='paid';},'App settlement not finished');
  assert.equal(appFinished.body.payment.clientToken,appPaid.payment.clientToken);
  const appCancel=await appCheckout('cancel');assert.ok([200,202].includes((await appApi(`/api/orders/${appCancel.orderId}/cancel`,'POST',{})).status));
  await waitFor(async()=> (await appApi(`/api/orders/${appCancel.orderId}`)).body.status==='cancelled','App cancellation incomplete');
  assert.equal((await appApi(`/api/checkouts/${appCancel.checkoutId}`)).status,409);
  assert.equal((await internal('payment','order',`/v1/payments/${appCancel.payment.id}`)).body.status,'cancelled');
  assert.equal((await internal('inventory','order',`/v1/reservations/${appCancel.reservationId}`)).body.status,'released');
  const appRefund=await appCheckout('refund');sql('inventory','UPDATE reservations SET expires_at=? WHERE id=?',[Date.now()-1000,appRefund.reservationId]);
  const appRefunded=await appApi(`/api/payments/${appRefund.payment.id}/confirm`,'POST',{});assert.ok([202,409].includes(appRefunded.status));
  await waitFor(async()=> (await appApi(`/api/orders/${appRefund.orderId}`)).body.status==='cancelled','App refund incomplete');
  assert.equal((await internal('payment','order',`/v1/payments/${appRefund.payment.id}`)).body.status,'refunded');
  assert.equal((await internal('inventory','order','/v1/stock/mug')).body.available,11);
  console.log('PASS: actual App BFF credentials, JWT, channel contract, 202 settlement, cancellation and refund');
  console.log('Settlement SIT passed');
 } finally {
  for (const service of [...processes.keys()].reverse()) await stop(service);
  for (const server of servers) { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
 }
}
