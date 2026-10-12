import { createHash, createHmac, randomBytes } from 'node:crypto';

export function serviceConfig(env = process.env) {
  const name = env.SERVICE_NAME;
  let keys;
  try { keys = JSON.parse(env.SERVICE_OUTGOING_KEYS); } catch { throw new Error('SERVICE_OUTGOING_KEYS required'); }
  if (!['desktop-bff', 'app-bff'].includes(name) || ['order', 'payment', 'audit-worker'].some(peer => typeof keys[peer] !== 'string' || Buffer.from(keys[peer], 'base64url').length !== 32)) throw new Error('Invalid service credentials');
  return { name, keys };
}
export function signInternalRequest(target, path, options, config = serviceConfig()) {
  const url = new URL(path, 'https://service.invalid');
  path = url.pathname + url.search;
  const key = config.keys[target];
  if (!key) throw new Error('Missing outbound service credential');
  const headers = { ...options.headers, 'x-service-id': config.name, 'x-service-time': String(Math.floor(Date.now() / 1000)), 'x-service-nonce': randomBytes(16).toString('hex') };
  const digest = createHash('sha256').update(options.body || '').digest('hex');
  const canonical = [options.method || 'GET', path, digest, config.name, target, headers['x-service-time'], headers['x-service-nonce'], headers['x-actor-id'] || '', headers['x-correlation-id'] || '', headers['x-user-scopes'] || '', headers['idempotency-key'] || ''].join('\n');
  headers['x-service-signature'] = createHmac('sha256', Buffer.from(key, 'base64url')).update(canonical).digest('hex');
  return headers;
}
