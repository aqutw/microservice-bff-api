# Identity and resource access

Both BFFs require a signed OAuth access token for protected routes. Configure
`AUTH_ISSUER`, `AUTH_AUDIENCE`, `AUTH_JWKS_URL` and optional `AUTH_ALGORITHMS`
(RS256 by default; ES256 and EdDSA are supported). Issuer, audience, signature,
subject, issuance time and expiry are checked. The owner identifier is a SHA-256
digest of issuer and subject, prefixed with `usr_`; body user IDs and actor headers
are never an identity source. Health and catalog remain public.

The website requires `PUBLIC_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` and a
32-byte base64url `SESSION_SECRET`. Register `/auth/callback` on that public
origin as an exact OIDC redirect URI and the public origin as a logout URI.
The provider must issue access tokens with the configured API audience.
Authorization Code with S256 PKCE, state and nonce is used. Access and ID tokens
stay in authenticated encrypted HttpOnly, SameSite=Lax cookies; HTTPS adds Secure.
Cookies are split when necessary and expire at the earlier of access-token expiry
or 30 minutes. Expiry requires signing in again; provider SSO can reuse its login.
No refresh tokens are persisted. `/auth/session` exposes user display data and a
session-bound CSRF token, never the bearer token. Browser mutations require both
the exact configured Origin and the CSRF token. Logout clears the local session
and requests provider token revocation / SSO logout when supported. Already issued
JWTs remain subject to the provider's revocation policy and their short expiry.

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
