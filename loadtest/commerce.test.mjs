import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
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
    __ENV: {}, __VU: 1, __ITER: 0, Counter: Metric, Rate: Metric,
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

test('payment success cannot hide unfinished order settlement', () => {
  const run = load(url => ({ status: url.includes('/confirm') ? 200 : 201,
    json: () => ({ payment: { id: 'p', status: 'paid' }, inventory: { reservationId: 'r' }, state:'settling',orderStatus:'settling' }) }));
  run.scenarios.desktopCheckout();
  assert.deepEqual(run.metrics.desktop_transactions_completed, [false]);
});

test('pending checkout and settlement are polled before counting completion', () => {
 let polls = 0;
 const run = load(url => ({status:202,json:()=>({checkoutId:'c1',state:url.includes('/confirm')?'settling':'processing'})}),
  () => { polls++; return {status:200,json:()=>({payment:{id:'p1',status:'paid'},inventory:{reservationId:'r1',status:'committed'},state:polls===1?'awaiting_payment':'paid',order:{status:'paid'}})}; });
 run.scenarios.desktopCheckout();
 assert.equal(polls,2);
 assert.deepEqual(run.metrics.desktop_transactions_completed,[true]);
});

test('load clients only use public BFF APIs and never commit inventory directly', () => {
 const run = load((url) => {
  assert.ok(!url.includes('/v1/'));
  return {status:url.includes('/confirm')?200:201,json:()=>({payment:{id:'p',status:'paid'},inventory:{reservationId:'r'},state:'paid',orderStatus:'paid'})};
 });
 run.scenarios.desktopCheckout();
 assert.deepEqual(run.metrics.desktop_transactions_completed,[true]);
});
