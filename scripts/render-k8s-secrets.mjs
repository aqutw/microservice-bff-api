import { X509Certificate, createPrivateKey } from 'node:crypto';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
process.loadEnvFile(process.argv[2] || '.env.security-production');
for (const name of ['AUTH_ISSUER', 'AUTH_JWKS_URL', 'PUBLIC_URL']) if (new URL(process.env[name]).protocol !== 'https:') throw new Error(`${name} must use HTTPS`);
if (process.env.DEPLOYMENT_ENV !== 'production') throw new Error('Production settings required; do not deploy local test credentials');
const peers = ['desktop-bff', 'app-bff', 'order', 'inventory', 'payment', 'audit-worker'];
const data = {};
for (const name of ['AUTH_ISSUER', 'AUTH_AUDIENCE', 'AUTH_JWKS_URL', 'PUBLIC_URL', 'OIDC_CLIENT_ID', 'OIDC_CLIENT_SECRET', 'SESSION_SECRET']) {
 if (!process.env[name]) throw new Error(`${name} required`); data[name] = process.env[name];
}
data.AUTH_ALGORITHMS = process.env.AUTH_ALGORITHMS || 'RS256';
if (data.AUTH_ALGORITHMS.split(',').some(value => !['RS256','ES256','EdDSA'].includes(value))) throw new Error('Unsupported access token algorithm');
if (Buffer.from(data.SESSION_SECRET, 'base64url').length !== 32) throw new Error('SESSION_SECRET must be 32 random bytes');
for (const peer of peers) for (const direction of ['INCOMING', 'OUTGOING']) {
 const key = `${peer.replaceAll('-', '_').toUpperCase()}_${direction}_KEYS`;
 if (!process.env[key] && (direction === 'OUTGOING' || !peer.endsWith('-bff'))) throw new Error(`${key} required`);
 if (process.env[key]) { const parsed = JSON.parse(process.env[key]); for (const value of Object.values(parsed)) if (Buffer.from(value,'base64url').length !== 32) throw new Error('Invalid service key'); data[key] = process.env[key]; }
}
const secret = (name, stringData) => ({ apiVersion: 'v1', kind: 'Secret', metadata: { name, namespace: 'commerce-poc' }, type: 'Opaque', stringData });
const secrets = [secret('commerce-security', data)];
const folder = process.env.SERVICE_PKI_DIR;
if (!folder) throw new Error('SERVICE_PKI_DIR required');
const ca = readFileSync(`${folder}/ca.pem`, 'utf8');
for (const peer of peers) {
 const certificate = readFileSync(`${folder}/${peer}.pem`, 'utf8'), privateKey = readFileSync(`${folder}/${peer}.key`, 'utf8');
 const parsed = new X509Certificate(certificate);
 if (!parsed.checkHost(peer, { subject: 'never' }) || !parsed.verify(new X509Certificate(ca).publicKey) || !parsed.checkPrivateKey(createPrivateKey(privateKey)) || Date.parse(parsed.validTo) <= Date.now()) throw new Error(`Invalid certificate for ${peer}`);
 secrets.push(secret(`commerce-${peer}-tls`, { 'tls.crt':certificate, 'tls.key':privateKey, 'ca.crt':ca }));
}
secrets.push({apiVersion:'v1',kind:'ConfigMap',metadata:{name:'commerce-service-ca',namespace:'commerce-poc'},data:{'ca.crt':ca}});
secrets.push(secret('commerce-website-ca', { 'ca.crt': ca }));
mkdirSync('.security', { recursive: true, mode: 0o700 });
writeFileSync('.security/k8s-secrets.json', JSON.stringify({ apiVersion: 'v1', kind: 'List', items: secrets }, null, 2), { mode: 0o600 });
console.log('Secrets written to the ignored .security/k8s-secrets.json; no secret values were printed.');
