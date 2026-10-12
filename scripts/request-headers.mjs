import { signInternalRequest } from '../bff-api/src/service-auth.js';
process.loadEnvFile(process.env.SECURITY_ENV_FILE || '.env.security');
process.loadEnvFile('.security/tokens.env');
const [target, caller, method, url, payload = '', correlation = '', idempotency = ''] = process.argv.slice(2);
const parsed = new URL(url);
const config = { name: caller, keys: JSON.parse(process.env[`${caller.replaceAll('-', '_').toUpperCase()}_OUTGOING_KEYS`]) };
const headers = signInternalRequest(target, parsed.pathname + parsed.search, { method, body: payload, headers: { 'x-actor-id': process.env.SIT_ACTOR, 'x-correlation-id': correlation, 'idempotency-key': idempotency } }, config);
console.log(Object.entries(headers).map(([name, value]) => `${name}: ${value}`).join('\n'));
