import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import test, { afterEach } from 'node:test';
import { request, readBody } from '../src/http.js';
import { checkout } from '../src/server.js';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

test('timeout bounds a stalled response body as well as response headers', async () => {
  globalThis.fetch = async (_, { signal }) => ({ ok: true, text: () => new Promise((_, reject) => signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true })) });
  await assert.rejects(request('http://upstream', '/', {}, 20), error => error.status === 504);
});

test('connection failure and invalid upstream JSON are gateway errors', async () => {
  globalThis.fetch = async () => { throw new Error('connection lost'); };
  await assert.rejects(request('http://upstream', '/'), error => error.status === 502);
  globalThis.fetch = async () => ({ ok: true, text: async () => 'not JSON' });
  await assert.rejects(request('http://upstream', '/'), error => error.status === 502);
});

test('request parser distinguishes malformed JSON and oversized UTF-8 bodies', async () => {
  const malformed = new EventEmitter();
  const parsed = readBody(malformed);
  malformed.emit('data', '{'); malformed.emit('end');
  await assert.rejects(parsed, error => error.status === 400);
  const oversized = new EventEmitter();
  const bounded = readBody(oversized, 4);
  oversized.emit('data', '中文字'); oversized.emit('end');
  await assert.rejects(bounded, error => error.status === 413);
});

test('checkout rejects non-object bodies and missing trusted identities', async () => {
  for (const input of [null, [], 123, {user_id: {}, items: [{sku: 'mug', quantity: 1}]}]) {
    await assert.rejects(checkout(input, 'desktop'), error => error.status === 400);
  }
});
