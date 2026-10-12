// Generates local-only credentials. No shared passwords or secrets enter Git.
import { execFileSync } from 'node:child_process';
import { chmodSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { mkdirSync, writeFileSync, existsSync } from 'node:fs';

const output = process.argv[2] || '.env.security';
if (existsSync(output)) throw new Error(`${output} already exists; use another path to preserve existing credentials`);
mkdirSync('.security', { recursive: true, mode: 0o700 });
mkdirSync('.security/pki', { recursive: true, mode: 0o700 });
execFileSync('openssl', ['req','-x509','-newkey','rsa:3072','-nodes','-keyout','.security/pki/ca.key','-out','.security/pki/ca.pem','-days','30','-subj','/CN=Commerce local test CA'], {stdio:'ignore'});
chmodSync('.security/pki/ca.key',0o600);
const secret = () => randomBytes(32).toString('base64url');
const peers = ['desktop-bff', 'app-bff', 'order', 'inventory', 'payment', 'audit-worker'];
for (const peer of peers) {
 const prefix = `.security/pki/${peer}`;
 execFileSync('openssl',['req','-newkey','rsa:2048','-nodes','-keyout',`${prefix}.key`,'-out',`${prefix}.csr`,'-subj',`/CN=${peer}`],{stdio:'ignore'});
 writeFileSync(`${prefix}.ext`,`subjectAltName=DNS:${peer},DNS:localhost,IP:127.0.0.1\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n`);
 execFileSync('openssl',['x509','-req','-in',`${prefix}.csr`,'-CA','.security/pki/ca.pem','-CAkey','.security/pki/ca.key','-CAcreateserial','-out',`${prefix}.pem`,'-days','30','-extfile',`${prefix}.ext`],{stdio:'ignore'});
 chmodSync(`${prefix}.key`,0o644);
}
const outgoing = Object.fromEntries(peers.map(peer => [peer, {}]));
const incoming = Object.fromEntries(peers.map(peer => [peer, {}]));
const connect = (caller, target) => { outgoing[caller][target] = incoming[target][caller] = secret(); };
for (const bff of ['desktop-bff', 'app-bff']) for (const target of ['order', 'payment', 'audit-worker']) connect(bff, target);
for (const target of ['inventory', 'payment', 'audit-worker']) connect('order', target);
connect('inventory', 'audit-worker'); connect('payment', 'audit-worker'); connect('payment', 'order');
const websitePort = process.env.WEBSITE_PORT || '3000';
const identityPort = process.env.IDENTITY_PORT || '9091';
const publicURL = `http://localhost:${websitePort}`;
const issuer = `http://localhost:${identityPort}/realms/commerce`;
const webSecret = secret(), cliSecret = secret(), shortSecret = secret();
const users = ['alice', 'bob', 'auditor'].map(username => ({ username, enabled: true, emailVerified: true, email: `${username}@example.test`, firstName: username, lastName: 'Local', realmRoles: username === 'auditor' ? ['audit-reader'] : [], credentials: [{ type: 'password', value: secret(), temporary: false }] }));
const audience = { name: 'commerce-api', protocol: 'openid-connect', protocolMapper: 'oidc-audience-mapper', config: { 'included.custom.audience': 'commerce-api', 'id.token.claim': 'false', 'access.token.claim': 'true' } };
const realm = {
 realm: 'commerce', enabled: true, sslRequired: 'none', registrationAllowed: false, bruteForceProtected: true, accessTokenLifespan: 300,
 roles: { realm: [{ name: 'audit-reader' }] }, users,
 clients: [
  { clientId: 'commerce-web', secret: webSecret, publicClient: false, protocol: 'openid-connect', standardFlowEnabled: true, directAccessGrantsEnabled: false, fullScopeAllowed: true, redirectUris: [`${publicURL}/auth/callback`], webOrigins: [publicURL], attributes: { 'pkce.code.challenge.method': 'S256', 'post.logout.redirect.uris': publicURL }, protocolMappers: [audience] },
  { clientId: 'commerce-short-test', secret: shortSecret, publicClient: false, protocol: 'openid-connect', standardFlowEnabled: false, directAccessGrantsEnabled: true, fullScopeAllowed: true, attributes: { 'access.token.lifespan': '3' }, protocolMappers: [audience] },
  { clientId: 'commerce-test', secret: cliSecret, publicClient: false, protocol: 'openid-connect', standardFlowEnabled: false, directAccessGrantsEnabled: true, fullScopeAllowed: true, protocolMappers: [audience] }
 ]
};
writeFileSync('.security/realm.json', JSON.stringify(realm, null, 2), { mode: 0o644 });
const env = { SERVICE_PKI_DIR: './.security/pki', DEPLOYMENT_ENV: 'development', AUTH_ALLOW_HTTP: 'true', AUTH_ISSUER: issuer, AUTH_AUDIENCE: 'commerce-api', AUTH_JWKS_URL: `${issuer}/protocol/openid-connect/certs`, PUBLIC_URL: publicURL, OIDC_CLIENT_ID: 'commerce-web', OIDC_CLIENT_SECRET: webSecret, SESSION_SECRET: secret(), TEST_CLIENT_SECRET: cliSecret, TEST_SHORT_CLIENT_SECRET: shortSecret, WEBSITE_PORT: websitePort, IDENTITY_PORT: identityPort, DESKTOP_PORT: process.env.DESKTOP_PORT || '3001', APP_PORT: process.env.APP_PORT || '3002', ORDER_PORT: process.env.ORDER_PORT || '8081', INVENTORY_PORT: process.env.INVENTORY_PORT || '8082', PAYMENT_PORT: process.env.PAYMENT_PORT || '8083', AUDIT_PORT: process.env.AUDIT_PORT || '8084' };
for (const peer of peers) {
 const prefix = peer.replaceAll('-', '_').toUpperCase();
 env[`${prefix}_INCOMING_KEYS`] = JSON.stringify(incoming[peer]);
 env[`${prefix}_OUTGOING_KEYS`] = JSON.stringify(outgoing[peer]);
}
for (const user of users) env[`TEST_${user.username.toUpperCase()}_PASSWORD`] = user.credentials[0].value;
writeFileSync(output, Object.entries(env).map(([key, value]) => `${key}='${value}'`).join('\n') + '\n', { mode: 0o600 });
console.log(`Local identity configuration written to ${output}; credentials were not printed.`);
