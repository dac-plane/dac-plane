# ADR-0007: Expose a versioned HTTP Surface API authenticated by external OpenID Connect

- Status: Proposed
- Date: 2026-10-22
- Issue: [#66](https://github.com/dac-plane/dac-plane/issues/66)

## Context

M1 proved the lifecycle through a transport-neutral command boundary
([`internal/application`](../../internal/application/application.go)). Every command already
carries an `identity.AuthorizationContext`, a client nonce bound to a payload fingerprint, and an
expected version where it changes a record. Nothing outside the process can reach that boundary:
there is no Surface API and no way to authenticate an external actor. Surfaces such as a web
workspace, chat, IDE, ticket or CI integrations cannot use the control plane until both exist.

Several inputs are already decided and are not reopened here:

- [ADR-0001](0001-control-plane-language.md) keeps public contracts language-neutral through
  OpenAPI 3.1 and JSON Schema 2020-12.
- [ADR-0002](0002-initial-application-boundary.md) places the Surface API, authentication and actor
  authorization inside the one control-plane application and forbids handlers that bypass
  lifecycle commands.
- The [pre-implementation review](../architecture/review-2026-10-15.md) requires every mutating
  surface command to be authenticated, authorized, idempotent and version checked, and requires
  replayable events whose acknowledgement is not lifecycle completion.

The operator model is open in the historical
[specification](../../architecture-explore/docs/spec/SPEC.md) ("measure before selecting self-host
vs managed"). Its identity rows state a position: authenticate people through an identity provider,
start with managed sign-in, add organization SSO later, and authenticate automated callers as scoped
services. The Plane-Bot evidence
([master record §5](../../architecture-explore/plane-bot-explore/notes/MASTER-RECORD.md),
[deep dive 01 §1](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-01.md)) shows a shipped
authority surface that is command-oriented and almost entirely request/response, streams only
transcripts and migrations, makes retries idempotent with a client nonce, and gives lifecycle
operations administrative twins on the same surface. Its trust topology, a vendor-owned backend and
sign-in service, is deliberately not adopted.

## Decision

### Operator neutrality

Design the Surface API and authentication so that the same artifact can be operated by a
self-hosting organization first and by a managed offering later. The operator configures which
identity providers are trusted. No tenant, issuer or credential is built into the product.

### Transport, contract and versioning

Serve the Surface API as JSON over HTTPS, described by one OpenAPI 3.1 document whose request,
response and event bodies reference the JSON Schema 2020-12 documents under `contracts/`.
Plaintext HTTP is not served except on loopback for development; where TLS terminates is decided
by the deployment ADR.

The path carries the major version (`/v1`). Within a major version changes are additive: new
optional response fields, new operations and new event kinds. Clients must ignore unknown response
fields and event kinds. The server rejects unknown fields in command bodies, so a client cannot
believe an unsupported option was applied. Removing or changing the meaning of a field requires a
new major version served alongside the old one for a stated deprecation period.

### Resource and command shape

Requests are tenant-scoped by path (`/v1/tenants/{tenant}/…`). Mutations are explicit commands that
map one-to-one onto application commands: submit a task, cancel it, decide an attention request,
assert spend, review a result, revoke authority, and the administrative migration commands. Reads
return the current snapshot of a record and its version. Each handler authenticates the caller,
builds the application command and calls the application function; it never reads or writes the
store directly and holds no transition rules. A read that does not yet exist in the application
layer is added there first.

Administrative operations, such as migration preview and execute, are part of the same API under
distinct permissions rather than a separate surface.

### Idempotency and versions

Every mutating request requires an `Idempotency-Key` header of 1 to 128 bytes, which becomes the
command nonce. A retry with the same key and payload returns the original outcome; the same key with
a different payload is refused as a conflict and leaves the original untouched. Responses carry the
record version as a strong `ETag`. A command that changes an existing record requires `If-Match`
with that version; a missing precondition is refused and a stale one is a conflict. These are the
existing application semantics expressed in HTTP, not new ones.

### Events

Expose committed lifecycle events as a replayable stream per task and per tenant using Server-Sent
Events, with a paged read of the same events for clients that poll. Each event carries an opaque,
ordered cursor as its SSE id. A client resumes with `Last-Event-ID` and receives every authorized
event after that cursor at least once, so it deduplicates by event identity. Delivery or
acknowledgement of an event is ingestion progress for the client; it is never treated as lifecycle
completion. The stream carries only events the principal is allowed to read.

The cursor encodes a stream generation and a per-tenant sequence. A cursor stays resumable for as
long as its events are retained. Durable events are product records, not a cache, and this
decision deletes none, so no time-based resumption window is promised. The server sends an
explicit reset event, after which the client re-reads the snapshot, when the cursor is malformed,
belongs to another tenant or stream, or carries a generation other than the current one. A restore
from backup or a future compaction advances the generation, so a cursor beyond what the database
now holds is detected instead of silently skipping events. Clients must handle a reset at any time.
A later retention decision sets the minimum resumable window together with data retention, without
changing this reset contract.

### Errors

Errors use Problem Details (RFC 9457) with stable type identifiers mapped from the application's
typed errors: invalid command, unauthorized, payload mismatch, version conflict, not found and the
typed lifecycle refusals. A record in another tenant and a record that does not exist return the
same response, so the API does not reveal cross-tenant resource existence. Error bodies never echo
credentials or token contents.

### Authentication of people

People authenticate with an external OpenID Connect provider chosen by the operator. dac-plane does
not implement sign-in and stores no passwords. The Surface API accepts bearer access tokens and
verifies each one locally:

- the issuer is one the operator configured, and its signing keys come from that issuer's published
  key set;
- the signature uses an algorithm the operator allowed for that issuer; unsigned tokens and
  symmetric algorithms with published keys are refused;
- the audience names this dac-plane API, and expiry and not-before hold within a small clock skew;
- the token lifetime does not exceed an operator-configured maximum, which bounds how long a
  revoked identity can keep acting.

An authenticated identity is the pair (issuer, subject). It maps to a principal only through an
explicit stored binding of (issuer, subject, tenant) to a principal. The tenant comes from the
request path and must match a binding; it is never taken from a token claim or a client header. An
email address or display name is not an identity. Identities without a binding are refused; there
is no just-in-time provisioning from token claims. Because a person who works in two tenants is two
principals, one external identity may hold one binding per tenant. Disabling a binding takes effect
on the next request.

Organization SSO is a configuration of the operator's identity provider or an additional trusted
issuer. It requires no change to this API.

### Authentication of machine surfaces

Automated surfaces, such as CI or ticket integrations, authenticate as service principals using
access tokens obtained from the same trusted identity provider with the OAuth 2.0 client
credentials grant. They are verified and bound exactly like people, with principal kind `service`.
dac-plane does not mint or store long-lived API keys under this decision. Provider adapters are not
Surface API clients; how they authenticate is decided with the adapter boundary (#67).

### Authorization

Authentication only establishes the principal. Authorization stays in the application layer:
the principal must belong to the request's tenant, and existing authority, permission and
administration checks apply unchanged. This ADR does not introduce a role model.

Reads are owner-scoped by default. A principal reads the tasks it owns and everything under them:
attempts, attention requests, events, artifacts, result revisions and reviews. Who may answer an
attention request remains the existing responder rule; reading and answering are separate checks.
Wider read access requires a narrow persisted tenant-reader permission, following the
harness-administrator pattern of [ADR-0004](0004-restricted-migration-administration.md): one row per
tenant and principal, set only through trusted provisioning, and checked under its row lock so a
revocation serializes with the reads it stops. The tenant-wide event stream requires that
permission; without it, a principal's stream carries only events of its own tasks. Team sharing is
left to a later decision.

### Provisioning

The Surface API cannot create tenants, principals, identity bindings or permissions. They are
created by a trusted provisioning command of the same binary, run by the operator with database
access. Each provisioning operation is idempotent and records the change with its time. In a
self-hosted deployment database access is already the root of trust, so the first binding needs no
pre-existing administrator. This formalizes the current state, where tenants, principals and
harness administrators exist only through store functions used in trusted setup. A Surface API
command for administering bindings may be added later, for a managed offering, behind its own
narrow permission and its own decision.

### Audit and secrets

Successful commands are audited through the existing lifecycle audit with the bound principal.
Authentication failures are logged with the issuer and a reason category, never with the token.
Tokens, authorization headers and key material are never written to a log, an audit record, a
durable event or an error body.

## Alternatives considered

- **ConnectRPC or gRPC with protobuf,** as in the Plane-Bot surface: rejected because it adds a
  second schema language beside the JSON Schema contracts ADR-0001 adopted, and it makes simple HTTP
  clients and the later web workspace depend on generated stubs. Its command-oriented,
  request/response shape is adopted.
- **GraphQL:** rejected because the API is command-oriented with per-command idempotency and
  version checks, which GraphQL mutations do not express more clearly than HTTP commands.
- **WebSockets for events:** rejected for the first version because events flow one way and SSE
  provides resumption by event id over ordinary HTTP.
- **dac-plane-owned sign-in and passwords:** rejected because it makes the control plane an identity
  provider, contradicts the specification's identity position and adds credential storage the
  product does not need.
- **Just-in-time principals from token claims:** rejected because any identity the provider issues
  would gain tenant access without an explicit grant.
- **Tenant from a token claim or header:** rejected because the binding, not the caller, must
  decide tenant membership.
- **dac-plane-issued API keys for machine surfaces:** deferred. Client credentials from the
  operator's identity provider give rotation and revocation without product-held secrets. Revisit
  only if an operator cannot provide them.
- **Tenant-wide reads for every principal:** rejected because it grants each principal, including
  narrowly scoped service principals, visibility of all tenant work without an explicit grant.
- **A general role model now:** rejected for the same reason ADR-0004 rejected one; a single narrow
  reader permission covers the current need without designing roles ahead of evidence.
- **First sign-in becomes administrator:** rejected because whoever reaches a fresh deployment first
  wins it, which is a race and a takeover path.
- **Bootstrap token from configuration:** rejected because it is a long-lived secret that ends up in
  configuration and logs.
- **Bindings declared in a configuration file:** deferred. It is attractive for repeatable setup,
  but the file and the database can drift with nothing reconciling them.
- **A fixed time-based cursor window:** rejected because no retention policy exists to enforce it;
  promising one would claim behavior nothing implements.

## Consequences

- #70 implements the OpenAPI document and handlers over application commands, and adds task,
  attempt and attention reads to the application layer rather than the handlers.
- `durable_event` is keyed by UUID with no ordered sequence, so #70 adds a monotonic per-tenant
  sequence and a stream generation before the event stream can provide resumable cursors.
- #70 filters every read and event stream by task ownership or the tenant-reader permission.
- #71 adds the identity binding and tenant-reader permission records with JSON Schemas and agreement
  tests, the trusted provisioning command, token verification against configured issuers, and
  refusal and audit tests, including cross-tenant, unbound and revoked identities.
- Data retention, including the minimum resumable event window, needs its own decision.
- The OpenAPI document becomes a public contract guarded in CI like the JSON Schemas: it must
  validate as OpenAPI 3.1 and reference only schemas that exist.
- Operating dac-plane requires an OpenID Connect provider. A development configuration may use a
  local test issuer; production configuration must not.
- A managed offering later can add tenants and issuers through the same binding record without
  changing the API.
