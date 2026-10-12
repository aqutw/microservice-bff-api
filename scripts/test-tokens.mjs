// Password grants are restricted to the generated local test client.
import { writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
process.loadEnvFile(process.argv[2] || '.env.security');
if (process.env.DEPLOYMENT_ENV !== 'development') throw new Error('Test grants are development only');
const issuer = process.env.AUTH_ISSUER;
for (let attempt = 0; attempt < 120; attempt++) {
  try { if ((await fetch(`${issuer}/.well-known/openid-configuration`, { signal: AbortSignal.timeout(2000) })).ok) break; } catch {}
  if (attempt === 119) throw new Error('Identity provider unavailable');
  await new Promise(resolve => setTimeout(resolve, 1000));
}
const values = {};
for (const user of ['alice', 'bob', 'auditor']) {
 const response = await fetch(`${issuer}/protocol/openid-connect/token`, { method: 'POST', body: new URLSearchParams({ grant_type: 'password', client_id: 'commerce-test', client_secret: process.env.TEST_CLIENT_SECRET, username: user, password: process.env[`TEST_${user.toUpperCase()}_PASSWORD`] }), signal: AbortSignal.timeout(5000) });
 if (!response.ok) throw new Error(`Test login rejected: ${user} (${response.status})`);
 const { access_token: token } = await response.json();
 const payload = JSON.parse(Buffer.from(token.split('.')[1], 'base64url'));
 values[`${user.toUpperCase()}_TOKEN`] = token;
 values[`${user.toUpperCase()}_ACTOR`] = `usr_${createHash('sha256').update(`${payload.iss}\0${payload.sub}`).digest('hex')}`;
}
values.SIT_TOKEN = values.AUDITOR_TOKEN; values.SIT_ACTOR = values.AUDITOR_ACTOR;
values.ACCESS_TOKEN = values.AUDITOR_TOKEN;
values.INVENTORY_SERVICE_KEY = JSON.parse(process.env.ORDER_OUTGOING_KEYS).inventory;
writeFileSync('.security/tokens.env', Object.entries(values).map(([key, value]) => `${key}=${value}`).join('\n') + '\n', { mode: 0o600 });
console.log('Test tokens prepared without printing credentials.');
