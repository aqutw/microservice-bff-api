import { randomBytes, timingSafeEqual } from 'node:crypto';
import { EncryptJWT, jwtDecrypt, createRemoteJWKSet, jwtVerify } from 'jose';
import * as oidc from 'openid-client';

const sessionCookie = 'commerce_session';
const transactionCookie = 'commerce_login';
let cachedConfig;
let cachedKey;

export function authSettings(env = process.env) {
  const development = env.DEPLOYMENT_ENV === 'development';
  const allowHTTP = development && env.AUTH_ALLOW_HTTP === 'true';
  const issuer = new URL(env.AUTH_ISSUER);
  const publicURL = new URL(env.PUBLIC_URL);
  const jwksURL = new URL(env.AUTH_JWKS_URL);
  for (const url of [issuer, publicURL, jwksURL]) {
    if (url.username || url.password || url.hash || !['https:', 'http:'].includes(url.protocol) || (!allowHTTP && url.protocol !== 'https:')) throw new Error('HTTPS required');
  }
  const key = Buffer.from(env.SESSION_SECRET || '', 'base64url');
  if (key.length !== 32 || !env.OIDC_CLIENT_ID || !env.OIDC_CLIENT_SECRET || !env.AUTH_AUDIENCE) throw new Error('Missing authentication settings');
  if ((env.AUTH_ALGORITHMS || 'RS256').split(',').some(value => !['RS256', 'ES256', 'EdDSA'].includes(value))) throw new Error('Unsupported access token algorithm');
  return { issuer, publicURL, jwksURL, key, allowHTTP, secure: publicURL.protocol === 'https:', clientId: env.OIDC_CLIENT_ID, clientSecret: env.OIDC_CLIENT_SECRET, audience: env.AUTH_AUDIENCE };
}

async function configuration(settings) {
  const cacheKey = JSON.stringify([settings.issuer.href, settings.clientId, settings.clientSecret, process.env.OIDC_INTERNAL_ORIGIN]);
  if (!cachedConfig || cachedKey !== cacheKey) {
    cachedKey = cacheKey;
    const internalOrigin = process.env.OIDC_INTERNAL_ORIGIN;
    if (internalOrigin && !settings.allowHTTP) throw new Error('Internal OIDC override is development only');
    const transport = (url, options) => {
      const target = new URL(url);
      if (internalOrigin && target.origin === settings.issuer.origin) target.host = new URL(internalOrigin).host;
      return fetch(target, { ...options, signal: AbortSignal.timeout(5000), redirect: 'error' });
    };
    cachedConfig = oidc.discovery(settings.issuer, settings.clientId, settings.clientSecret, undefined, {
      timeout: 5, [oidc.customFetch]: transport,
      execute: settings.allowHTTP ? [oidc.allowInsecureRequests] : []
    }).catch(error => { cachedConfig = undefined; throw error; });
  }
  return cachedConfig;
}

async function seal(payload, purpose, settings, ttl) {
  return new EncryptJWT(payload).setProtectedHeader({ alg: 'dir', enc: 'A256GCM' })
    .setIssuer(settings.publicURL.origin).setAudience(purpose).setIssuedAt().setExpirationTime(Math.floor(Date.now() / 1000) + ttl).encrypt(settings.key);
}
async function unseal(value, purpose, settings) {
  return (await jwtDecrypt(value, settings.key, { issuer: settings.publicURL.origin, audience: purpose, keyManagementAlgorithms: ['dir'], contentEncryptionAlgorithms: ['A256GCM'] })).payload;
}
function cookie(request, name) {
  return (request.headers.get('cookie') || '').split(';').map(part => part.trim()).find(part => part.startsWith(`${name}=`))?.slice(name.length + 1);
}
function setCookie(response, name, value, settings, ttl) {
  response.headers.append('set-cookie', `${name}=${value}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${ttl}${settings.secure ? '; Secure' : ''}`);
}
function clearSession(response, settings) {
  for (let part = 0; part < 4; part++) setCookie(response, part ? `${sessionCookie}.${part}` : sessionCookie, '', settings, 0);
}
export async function readSession(request, settings = authSettings()) {
  try {
    let value = cookie(request, sessionCookie);
    if (!value) return null;
    for (let part = 1; part < 4; part++) value += cookie(request, `${sessionCookie}.${part}`) || '';
    if (value.length > 14400) return null;
    const session = await unseal(value, 'session', settings);
    return typeof session.accessToken === 'string' && typeof session.csrf === 'string' ? session : null;
  } catch { return null; }
}
export function validMutation(request, session, settings = authSettings()) {
  const provided = request.headers.get('x-csrf-token') || '';
  const expected = session?.csrf || '';
  return request.headers.get('origin') === settings.publicURL.origin && expected.length > 0 && provided.length === expected.length && timingSafeEqual(Buffer.from(provided), Buffer.from(expected));
}
function response(body, status = 200) {
  return Response.json(body, { status, headers: { 'cache-control': 'no-store', 'referrer-policy': 'no-referrer' } });
}

export async function authRoute(request, action) {
  let settings;
  try { settings = authSettings(); } catch { return response({ error: 'auth_not_configured' }, 503); }
  if (action === 'session' && request.method === 'GET') {
    const session = await readSession(request, settings);
    return response(session ? { authenticated: true, user: session.user, csrfToken: session.csrf } : { authenticated: false });
  }
  if (action === 'logout' && request.method === 'POST') {
    const session = await readSession(request, settings);
    if (!session) return response({ error: 'unauthorized' }, 401);
    if (!validMutation(request, session, settings)) return response({ error: 'csrf_rejected' }, 403);
    let logoutURL = settings.publicURL.origin;
    try {
      const config = await configuration(settings);
      if (config.serverMetadata().revocation_endpoint) await oidc.tokenRevocation(config, session.accessToken);
      if (config.serverMetadata().end_session_endpoint) logoutURL = oidc.buildEndSessionUrl(config, { id_token_hint: session.idToken, post_logout_redirect_uri: settings.publicURL.origin }).href;
    } catch { /* Local logout still removes the browser session during an IdP outage. */ }
    const result = response({ loggedOut: true, logoutURL });
    clearSession(result, settings);
    return result;
  }
  if (request.method !== 'GET' || !['login', 'callback'].includes(action)) return response({ error: 'not_found' }, 404);
  try {
    const config = await configuration(settings);
    if (action === 'login') {
      const returnTo = new URL(request.url).searchParams.get('returnTo') === '/audit' ? '/audit' : '/';
      const transaction = { verifier: oidc.randomPKCECodeVerifier(), state: oidc.randomState(), nonce: oidc.randomNonce(), returnTo };
      const url = oidc.buildAuthorizationUrl(config, {
        redirect_uri: `${settings.publicURL.origin}/auth/callback`, scope: 'openid profile', state: transaction.state, nonce: transaction.nonce,
        code_challenge: await oidc.calculatePKCECodeChallenge(transaction.verifier), code_challenge_method: 'S256'
      });
      const result = new Response(null, { status: 302, headers: { location: url.href, 'cache-control': 'no-store' } });
      setCookie(result, transactionCookie, await seal(transaction, 'login', settings, 300), settings, 300);
      return result;
    }
    const transaction = await unseal(cookie(request, transactionCookie) || '', 'login', settings);
    // Use configured public origin, never an untrusted Host or forwarded header.
    const callback = new URL(`${settings.publicURL.origin}/auth/callback`);
    callback.search = new URL(request.url).search;
    const tokens = await oidc.authorizationCodeGrant(config, callback, { pkceCodeVerifier: transaction.verifier, expectedState: transaction.state, expectedNonce: transaction.nonce, idTokenExpected: true });
    const { payload } = await jwtVerify(tokens.access_token, createRemoteJWKSet(settings.jwksURL, { timeoutDuration: 2500 }), {
      issuer: process.env.AUTH_ISSUER, audience: settings.audience, algorithms: (process.env.AUTH_ALGORITHMS || 'RS256').split(','), requiredClaims: ['sub', 'exp', 'iat']
    });
    if (payload.sub !== tokens.claims()?.sub) throw new Error('subject_mismatch');
    const ttl = Math.min(1800, Math.floor(payload.exp - Date.now() / 1000));
    if (ttl <= 0) throw new Error('expired_token');
    const user = { name: tokens.claims().name || tokens.claims().preferred_username || payload.sub, auditRead: (payload.scope || '').split(' ').includes('audit:read') || payload.realm_access?.roles?.includes('audit-reader') || false };
    const sealed = await seal({ accessToken: tokens.access_token, idToken: tokens.id_token, user, csrf: randomBytes(32).toString('base64url') }, 'session', settings, ttl);
    if (sealed.length > 14400) throw new Error('session_too_large');
    const result = new Response(null, { status: 302, headers: { location: new URL(transaction.returnTo, settings.publicURL).href, 'cache-control': 'no-store', 'referrer-policy': 'no-referrer' } });
    clearSession(result, settings);
    for (let part = 0; part < Math.ceil(sealed.length / 3600); part++) setCookie(result, part ? `${sessionCookie}.${part}` : sessionCookie, sealed.slice(part * 3600, (part + 1) * 3600), settings, ttl);
    setCookie(result, transactionCookie, '', settings, 0);
    return result;
  } catch {
    const result = response({ error: action === 'callback' ? 'login_failed' : 'identity_unavailable' }, action === 'callback' ? 401 : 503);
    setCookie(result, transactionCookie, '', settings, 0);
    return result;
  }
}
