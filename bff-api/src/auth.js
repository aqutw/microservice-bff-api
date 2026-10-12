import { createHash } from 'node:crypto';
import { createRemoteJWKSet, jwtVerify } from 'jose';
import { apiError } from './http.js';

export function identityConfig(env = process.env) {
  const issuer = env.AUTH_ISSUER;
  const audience = env.AUTH_AUDIENCE;
  const jwksURL = env.AUTH_JWKS_URL;
  if (!issuer || !audience || !jwksURL) throw new Error('AUTH_ISSUER, AUTH_AUDIENCE and AUTH_JWKS_URL are required');
  const allowHTTP = env.DEPLOYMENT_ENV === 'development' && env.AUTH_ALLOW_HTTP === 'true';
  for (const value of [issuer, jwksURL]) {
    const url = new URL(value);
    if (url.username || url.password || url.hash || (!allowHTTP && url.protocol !== 'https:') || !['https:', 'http:'].includes(url.protocol)) throw new Error('Identity endpoints require HTTPS');
  }
  const algorithms = (env.AUTH_ALGORITHMS || 'RS256').split(',');
  if (algorithms.some(algorithm => !['RS256', 'ES256', 'EdDSA'].includes(algorithm))) throw new Error('Unsupported access token algorithm');
  return { issuer, audience, jwksURL, algorithms };
}

export function createAuthenticator(config = identityConfig(), keySet) {
  const keys = keySet || createRemoteJWKSet(new URL(config.jwksURL), { timeoutDuration: 2500, cacheMaxAge: 300000 });
  return async req => {
    const authorization = req.headers.authorization;
    if (typeof authorization !== 'string' || authorization.length > 8192 || !/^Bearer \S+$/i.test(authorization)) throw apiError('unauthorized', 401, false);
    try {
      const { payload } = await jwtVerify(authorization.slice(7), keys, {
        issuer: config.issuer, audience: config.audience, algorithms: config.algorithms,
        requiredClaims: ['iss', 'aud', 'sub', 'exp', 'iat'], clockTolerance: 5
      });
      if (typeof payload.sub !== 'string' || !payload.sub || payload.sub.length > 256 || payload.iat > Date.now() / 1000 + 5 || (payload.typ && payload.typ !== 'Bearer')) throw new Error('invalid_subject');
      const id = `usr_${createHash('sha256').update(`${payload.iss}\0${payload.sub}`).digest('hex')}`;
      const scopes = typeof payload.scope === 'string' ? payload.scope.split(/\s+/) : [];
      const roles = Array.isArray(payload.realm_access?.roles) ? payload.realm_access.roles : [];
      return { id, subject: payload.sub, scopes, auditRead: scopes.includes('audit:read') || roles.includes('audit-reader') };
    } catch (error) {
      if (error.code === 'ERR_JWKS_TIMEOUT' || error instanceof TypeError) throw apiError('identity_unavailable', 503);
      throw apiError('unauthorized', 401, false);
    }
  };
}
