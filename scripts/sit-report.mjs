import { copyFileSync, existsSync, lstatSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// Only known process logs leave the temporary credential/database workspace.
export function writeSettlementReport(workspace, destination, metadata) {
 mkdirSync(destination, { recursive: true });
 for (const name of ['audit-worker.log', 'inventory.log', 'payment.log', 'order.log', 'app-bff.log']) {
  const path = join(workspace, name);
  if (existsSync(path) && lstatSync(path).isFile()) copyFileSync(path, join(destination, name));
 }
 const result = {
  sourceRevision: metadata.sourceRevision,
  workingTreeDirty: metadata.workingTreeDirty === true,
  success: metadata.success === true,
  startedAt: metadata.startedAt,
  completedAt: new Date().toISOString(),
  checks: ['settlement-sit.mjs']
 };
 writeFileSync(join(destination, 'result.json'), JSON.stringify(result, null, 2));
}
