import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { writeSettlementReport } from '../sit-report.mjs';

for (const success of [false, true]) test(`SIT ${success ? 'success' : 'failure'} artifacts exclude credentials, databases and unexpected files`, t => {
 const root = mkdtempSync(join(tmpdir(), 'sit-report-test-'));
 t.after(() => rmSync(root, { recursive: true, force: true }));
 const source = join(root, 'source'), report = join(root, 'report');
 mkdirSync(source);
 for (const name of ['.env.security', 'tokens.env', 'private.key', 'order.db', 'unexpected.log']) writeFileSync(join(source, name), 'private-test-material');
 writeFileSync(join(source, 'order.log'), 'checkout will retry');
 // Even an allowed log name cannot be used to follow a credential symlink.
 symlinkSync(join(source, 'private.key'), join(source, 'payment.log'));
 writeSettlementReport(source, report, { sourceRevision: 'test-revision', success, startedAt: '2026-10-14T00:00:00Z', privateKey: 'private-test-material' });
 assert.deepEqual(readdirSync(report).sort(), ['order.log', 'result.json']);
 assert.equal(readFileSync(join(report, 'order.log'), 'utf8'), 'checkout will retry');
 const result = JSON.parse(readFileSync(join(report, 'result.json'), 'utf8'));
 assert.equal(result.success, success); assert.equal(result.sourceRevision, 'test-revision');
 assert.deepEqual(result.checks, ['settlement-sit.mjs']);
 assert.equal(readFileSync(join(report, 'result.json'), 'utf8').includes('private-test-material'), false);
});
