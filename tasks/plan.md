# Issue #71 — Authenticate surface actors and map them to principals

Tracker: https://github.com/dac-plane/dac-plane/issues/71
Branch: `feature/71-actor-authentication`, from `main` at `b27ba92`.
Parent: #65 (M2: Deployable control plane).

## Direction

Implement the authentication half of [ADR-0007](../docs/adr/0007-surface-api-transport-and-authentication.md)
as a transport-neutral boundary: given a bearer token and the tenant named by a request, produce an
`identity.AuthorizationContext` or a uniform refusal. #70 calls it from HTTP handlers; nothing here
adds an HTTP surface. Authorization stays in `internal/application`.

Decisions confirmed with the maintainer on 2026-10-23:

- Token verification uses `github.com/coreos/go-oidc/v3` (v3.21.0, with `go-jose/v4`): discovery,
  key-set caching and rotation, pinned algorithms, audience and expiry. Checks it lacks are added on
  top.
- #71 ships a minimal `dac-plane provision` subcommand; running with no subcommand keeps today's
  behavior. #73 adds `serve`, `migrate` and `health` around it.
- Trusted issuers come from a JSON file named by `DAC_IDENTITY_CONFIG_FILE`, validated against a
  JSON Schema under `contracts/`.
- Access tokens must be typed `at+jwt` (RFC 9068). An operator may opt one issuer out; that issuer
  then gets RFC 8725 mutually exclusive validation (dedicated audience, ID-token-only claims
  `nonce` and `at_hash` rejected), and the opt-out is logged at startup.

Industry practice checked (Tavily, 2026-10-23) and adopted:

- RFC 9068 §4: require `typ` `at+jwt` or `application/at+jwt`; exact `iss` match; `aud` must contain
  this API's identifier; signature with the issuer's keys only; reject `alg` `none`.
- RFC 8725 and its 2026 revision draft: pin algorithms per issuer and never trust the token's `alg`;
  treat algorithm names case-sensitively; never follow `jku`, `x5u` or embedded `jwk` headers; reject
  JWE and non-compact serializations; explicit typing and mutually exclusive validation.
- OpenID Connect Core §5.7: (`iss`, `sub`) is the only stable identifier; bind on it exactly,
  case-sensitive, never on email or username. `sub` is at most 255 ASCII characters.
- Key-set refresh on an unknown `kid` is attacker-triggerable; coalesce and rate-limit refreshes.
  go-oidc's `RemoteKeySet` refetches on every unknown key ID, so it is wrapped.
- Keep access-token lifetimes short; enforce a per-issuer maximum (`exp - iat`) with `iat` required.

Evidence from `architecture-explore`: the TypeScript prototype maps an identity-provider subject to a
principal (`services/product-tasks/migrations/0001_product_core.sql`) but keys on `sub` alone, which
ADR-0007 deliberately replaces with (issuer, subject, tenant). Grok Bot uses its vendor's sign-in and
offers no reusable pattern.

## Implementation

1. **Identity records** (`internal/identity`)
   - [x] `IdentityBinding`: tenant, principal, issuer (https URL, no query or fragment), subject
     (1–255 ASCII), enabled, updated time; `Validate()`.
   - [x] `TenantReader`: tenant, principal, enabled, updated time; `Validate()`.
   - [x] JSON Schemas `identity/identity-binding.schema.json` and `identity/tenant-reader.schema.json`
     with `GoContract` agreement tests.
2. **Issuer configuration** (`internal/authn`)
   - [x] `Config` with one entry per issuer: issuer, audience, allowed algorithms (asymmetric only),
     maximum token lifetime, clock skew, `require_access_token_type` (default true).
   - [x] Schema `config/identity-config.schema.json` with an agreement test; loaded from
     `DAC_IDENTITY_CONFIG_FILE`; errors never echo file contents.
3. **Persistence** (`internal/platform`)
   - [x] Migration `00010_actor_identity.sql`: `identity_binding` keyed by (issuer, subject, tenant)
     with a foreign key to `principal`; `tenant_reader` keyed by (tenant, principal);
     append-only `provisioning_change` (operation, target identifiers, time, no secrets). Downgrade
     refuses when bindings exist, following the existing evidence-preserving pattern.
   - [x] Store functions: upsert and disable a binding, resolve (issuer, subject, tenant) to an
     enabled principal, set and read the reader permission under a row lock, record changes.
4. **Verification** (`internal/authn`)
   - [x] One verifier per configured issuer via go-oidc discovery; keys only from the discovered
     `jwks_uri`, wrapped by a rate-limited key set (coalesced refresh, minimum interval on unknown
     `kid`).
   - [x] Before signature checks: compact JWS only, size limit, `typ` check unless opted out.
   - [x] After go-oidc checks (signature, pinned algorithm, `iss`, `aud`, `exp`/`nbf` with skew):
     `iat` required, `exp - iat` within the maximum, `sub` shape, and in opt-out mode no `nonce` or
     `at_hash`.
   - [x] `Authenticate(ctx, token, tenant)` returns an `identity.AuthorizationContext` from the
     binding, or one uniform `ErrUnauthenticated`. Unbound, disabled and other-tenant identities are
     indistinguishable to the caller. Internally a reason category (missing, malformed,
     untrusted_issuer, wrong_type, bad_signature, wrong_audience, expired, not_yet_valid,
     lifetime_exceeded, invalid_claims, unbound) is logged with the issuer only.
5. **Provisioning**
   - [x] Idempotent functions: create tenant, create principal (human or service), bind and unbind an
     identity, grant and revoke tenant reader; each records a `provisioning_change` in the same
     transaction.
   - [x] `dac-plane provision <tenant|principal|bind|unbind|grant-reader|revoke-reader>` with flags,
     JSON output of identifiers; no subcommand keeps the current process behavior.
6. **Tests and CI**
   - [x] Unit tests against an in-test TLS issuer (discovery and key set from `httptest`) with
     generated keys: valid token; `alg` `none`; HS256 with the public key (key confusion); algorithm
     not allowed; mixed-case `alg`; wrong and missing `typ`; ID token substitution in opt-out mode;
     wrong `iss`; missing and wrong `aud`; expired; not yet valid; skew edges; missing `iat`; lifetime
     exceeded; oversized and JWE tokens; `jku` header ignored; key rotation; unknown-`kid` flood keeps
     key-set fetches bounded.
   - [x] Integration tests (PostgreSQL): resolve binding; disabled binding refused on the next call;
     same identity bound in two tenants resolves per tenant; cross-tenant and unbound give the same
     error; reader grant and revoke; provisioning idempotency and change records; downgrade refusal.
   - [x] Log-capture test: no token, header or key material in any log line.
   - [x] `just test-identity` and `just test-identity-race` recipes; confirm `just ci` covers them.
   - [x] `just vuln` clean with the new dependencies.

## Deviations from the plan

- `nonce` and `at_hash` are refused for every issuer, not only for issuers that opt out of
  `at+jwt` typing: an access token never needs them, so the stricter rule costs nothing.
- `enabled` is optional in the binding and reader schemas and absent means disabled, matching Go's
  zero value so schema and Go agree; the safe default is no access.
- `authn.StoreResolver` adapts the PostgreSQL binding lookup to the verifier, so #70 wires
  authentication without new store code.
- `DAC_IDENTITY_CONFIG_FILE` is read into `config.Config` as an optional path; the process that serves
  the Surface API (#70, #73) loads it with `authn.LoadConfig`.

## Verification

- `just test-contracts`, `just test-identity`, `just test-identity-race`, `just ci`
- `git diff --check`

## Remaining boundaries

No HTTP handlers, Surface API routes or read filtering (#70); no adapter registrations or provider
bindings (#72); no `serve`, `migrate` or `health` subcommands (#73). No identity provider is built and
no password is stored. Role models and team sharing stay out of scope (ADR-0007).
