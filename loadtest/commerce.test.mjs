import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { createHash, createHmac, randomBytes } from 'node:crypto';
import test from 'node:test';

function load(post, get) {
  const metrics = {};
  class Metric {
    constructor(name) { this.samples = metrics[name] = []; }
    add(value) { this.samples.push(value); }
  }
  const source = readFileSync(new URL('./commerce.js', import.meta.url), 'utf8')
    .replace(/^import .*;$/gm, '').replace(/export /g, '');
  const context = vm.createContext({
    __ENV: {INVENTORY_SERVICE_KEY:Buffer.alloc(32,5).toString('base64url')}, Uint8Array, crypto: {randomBytes, sha256:(value)=>createHash('sha256').update(value).digest('hex'), hmac:(_,key,value)=>createHmac('sha256',key).update(value).digest('hex')}, encoding:{b64decode:value=>Buffer.from(value,'base64url')}, __VU: 1, __ITER: 0, Counter: Metric, Rate: Metric,
    http: { post, get }, sleep() {},
    check: (response, checks) => Object.values(checks).every(fn => fn(response))
  });
  vm.runInContext(`${source}\nglobalThis.scenarios = {desktopCheckout, appCheckout}; globalThis.thresholds = options.thresholds;`, context);
  return { ...context, metrics };
}

test('every failed checkout contributes a failed transaction despite successful reads', () => {
  const run = load(() => ({ status: 502, json: () => ({}) }));
  for (let i = 0; i < 15; i++) run.scenarios.desktopCheckout();
  for (let i = 0; i < 6; i++) run.scenarios.appCheckout();
  assert.equal(run.metrics.desktop_transactions_completed.length, 15);
  assert.equal(run.metrics.app_transactions_completed.length, 6);
  assert.ok(Object.values(run.metrics).filter(samples => typeof samples[0] === 'boolean').every(samples => samples.every(value => !value)));
  assert.equal(run.thresholds.desktop_transactions_completed[0], 'rate==1');
  assert.equal(run.thresholds.app_transactions_completed[0], 'rate==1');
});

test('payment success cannot hide a failed inventory commit', () => {
  const run = load(url => ({ status: url.includes('/commit') ? 500 : url.includes('/confirm') ? 200 : 201,
    json: () => ({ payment: { id: 'p', status: 'paid' }, inventory: { reservationId: 'r' } }) }));
  run.scenarios.desktopCheckout();
  assert.deepEqual(run.metrics.desktop_transactions_completed, [false]);
});

test('a pending checkout is polled before confirming and committing', () => {
 let polls = 0;
 const run = load(url => ({status: url.includes('/confirm') || url.includes('/commit') ? 200 : 202,
  json: () => url.includes('/confirm') ? {payment:{status:'paid'}} : url.includes('/commit') ? {status:'committed'} : {checkoutId:'c1',state:'processing'}}),
  () => { polls++; return {status:200,json:()=>({payment:{id:'p1'},inventory:{reservationId:'r1'}})}; });
 run.scenarios.desktopCheckout();
 assert.equal(polls,1);
 assert.deepEqual(run.metrics.desktop_transactions_completed,[true]);
});

test('internal inventory calls are authenticated with a recipient-bound signature', () => {
 let committed = false;
 const run = load((url,body,options) => {
  if (url.includes('/commit')) {
   const h = options.headers;
   assert.equal(h['Authorization'],undefined);assert.equal(h['X-Service-ID'],'order');
   const canonical = ['POST','/v1/reservations/r/commit',createHash('sha256').update('').digest('hex'),'order','inventory',h['X-Service-Time'],h['X-Service-Nonce'],'',h['X-Correlation-ID'],'',''].join('\n');
   assert.equal(h['X-Service-Signature'],createHmac('sha256',Buffer.alloc(32,5)).update(canonical).digest('hex'));
   committed = true;
  }
  return {status:url.includes('/confirm') || url.includes('/commit') ? 200 : 201,json:()=>({payment:{id:'p',status:'paid'},inventory:{reservationId:'r'},status:'committed'})};
 });
 run.scenarios.desktopCheckout();assert.equal(committed,true);
});
