// A durable registry for this PoC's single website replica. Tokens are encrypted
// before storage; cookie IDs are random and restricted to one private directory.
import { mkdir, readdir, readFile, writeFile, unlink } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
import { join } from 'node:path';

const pattern = /^(\d{10})-[a-f0-9]{64}$/;
function unavailable() { return Object.assign(new Error('session_store_unavailable'), { status: 503 }); }
export async function storeSession(directory, ciphertext, ttl) {
 try {
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const files = await readdir(directory), now = Math.floor(Date.now() / 1000);
  let active = 0;
  for (const file of files) {
   const match = file.match(pattern);
   if (!match) continue;
   if (Number(match[1]) <= now) await unlink(join(directory, file)).catch(error => { if (error.code !== 'ENOENT') throw error; });
   else active++;
  }
  if (active >= 10000 || ciphertext.length > 65536) throw unavailable();
  const record = `${now + ttl}-${randomBytes(32).toString('hex')}`;
  await writeFile(join(directory, record), ciphertext, { flag: 'wx', mode: 0o600 });
  return record;
 } catch { throw unavailable(); }
}
export async function loadSession(directory, record) {
 const match = typeof record === 'string' && record.match(pattern);
 if (!match || Number(match[1]) <= Date.now() / 1000) return null;
 try {
  const value = await readFile(join(directory, record), 'utf8');
  return value.length <= 65536 ? value : null;
 } catch (error) { if (error.code === 'ENOENT') return null; throw unavailable(); }
}
export async function deleteSession(directory, record) {
 if (!pattern.test(record || '')) return;
 try { await unlink(join(directory, record)); }
 catch (error) { if (error.code !== 'ENOENT') throw unavailable(); }
}
