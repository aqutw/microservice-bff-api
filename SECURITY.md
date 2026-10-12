# Identity and resource access

Both BFFs require a signed OAuth access token for protected routes. Configure
`AUTH_ISSUER`, `AUTH_AUDIENCE`, `AUTH_JWKS_URL` and optional `AUTH_ALGORITHMS`
(RS256 by default; ES256 and EdDSA are supported). Issuer, audience, signature,
subject, issuance time and expiry are checked. The owner identifier is a SHA-256
digest of issuer and subject, prefixed with `usr_`; body user IDs and actor headers
are never an identity source. Health and catalog remain public.

The website requires `PUBLIC_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, a
32-byte base64url `SESSION_SECRET`, and a private persistent `SESSION_STORE_DIR`.
Register `/auth/callback` on that public origin as an exact OIDC redirect URI and
the public origin as a logout URI. The provider must issue access tokens with the
configured API audience. Authorization Code with S256 PKCE, state and nonce is
used. Access and ID tokens are encrypted in a server-side registry; an encrypted
HttpOnly, SameSite=Lax cookie contains only a random session reference. HTTPS adds
Secure and the `__Host-` prefix to prevent subdomain cookie injection. Sessions
expire at the earlier of access-token expiry or 30 minutes; no refresh tokens are
persisted. The registry uses atomic file creation and removal, private file modes,
a bounded session count and expired-record cleanup for this single-replica PoC.
Shared storage or a dedicated session service is required before scaling the
website across independent hosts.

`/auth/session` exposes user display data and a session-bound CSRF token, never
bearer tokens. Browser mutations require the exact configured Origin and CSRF
token. Logout deletes the durable server record before clearing cookies, making
retained copies immediately invalid. Provider revocation / SSO logout is requested
when supported. Separately issued App JWTs remain subject to the provider's
revocation policy and their expiry. Registry outages reject protected operations.

App clients send `Authorization: Bearer <access token>` directly to the App BFF.
Web APIs accept the encrypted website session, replacing client-supplied identity
headers. Configure `ALLOWED_ORIGINS` for intentional cross-origin BFF clients.
Responses containing user data are not cacheable.

Production identity endpoints and the public website require HTTPS. Plain HTTP
is available only with both `DEPLOYMENT_ENV=development` and `AUTH_ALLOW_HTTP=true`.
`OIDC_INTERNAL_ORIGIN` is a development-only transport mapping for a containerized
provider; the public issuer and browser URLs remain unchanged. Missing identity
configuration fails closed. Never deploy a development issuer to production.

The payment confirmation endpoint is a PoC payment simulation. Production payment
settlement requires a provider integration and authenticated provider callbacks.

## Resource permissions

| Resource | Permission |
| --- | --- |
| Health / catalog | Public |
| Create checkout | Authenticated user; owner is derived from identity |
| Read order / checkout | Exact owner, enforced in BFF and Order |
| Confirm simulated payment | Exact owner, checked before any mutation |
| Read audit | `audit:read` scope or trusted `audit-reader` realm role |
| Inventory / domain mutations | Authorized internal service only |

Ownership violations return 404 to avoid disclosing resource existence. Audit
permission failures return 403. Admin/audit privileges do not grant ownership of
other users' purchases. Checkout idempotency namespaces include owner, channel
and client key. Existing workflows retain their IDs so recovery remains possible;
legacy `demo-user` / arbitrary user IDs are not automatically assigned to a new
login. Explicitly reviewed identity mappings are required before migrating old
owners. New owner namespaces prevent replaying an old anonymous workflow.

## Internal service trust and deployment

Every internal listener and caller requires TLS 1.3 with the configured service
CA. Configure `SERVICE_TLS_CERT`, `SERVICE_TLS_KEY`, `SERVICE_CA_FILE`; Node callers
also use `NODE_EXTRA_CA_CERTS`. No verification bypass is offered. HTTPS protects
responses and delegated metadata as well as the signed request. External browser
HTTPS terminates at the production Gateway; local browser HTTP is explicit.

`SERVICE_NAME`, `SERVICE_INCOMING_KEYS` and `SERVICE_OUTGOING_KEYS` contain per-peer
base64url signing-key maps. Keys are independent for each directed connection, so
an Inventory recipient cannot impersonate Order to Payment. HMAC-SHA256 signs a
canonical request including recipient, body digest, actor, scopes and idempotency
key. A 30-second clock window and one-use nonce prevent request replay. Receivers
persist bounded nonce records in their SQLite databases across restarts; fresh retries use new signatures while application
idempotency still prevents duplicate mutations. Synchronize service clocks.

Only BFFs can submit checkouts. Order can reserve/release/commit inventory and
create/read payments; it cannot use the simulated confirmation endpoint. BFFs can
confirm a payment only after owner checks, and Payment checks again through Order.
Audit publishers can submit events only for their own service. Audit readers need
both an authenticated BFF caller and signed audit permission.

The initializer creates independent random development keys and PKI; production
config must provide its own credentials and certificates. Base Compose keeps
internal ports unpublished. Kubernetes policies restrict service traffic, disable
service-account token mounting, drop capabilities and use non-root processes.
Production deployment requires a NetworkPolicy-capable CNI, HTTPS Gateway and
matching service certificates. Deployment Secrets are rendered to an ignored,
mode-0600 file, never committed or printed. Rotate peer keys on both endpoints;
rotate the session encryption key to invalidate browser sessions. Signing keys,
TLS certificates and provider JWKS have separate lifecycles.
