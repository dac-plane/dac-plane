# ADR-0008: Run provider adapters out of process over an adapter-initiated ControlFrame connection

- Status: Accepted
- Date: 2026-10-22
- Issue: [#67](https://github.com/dac-plane/dac-plane/issues/67)

## Context

[ADR-0003](0003-provider-contract-semantics.md) defines `ControlFrame` as the logical adapter protocol
"across an external process boundary": versioned lifecycle, request, cancel, reply and event frames;
fail-fast version and capability negotiation; typed failures; replayable event cursors. M1 implemented
those semantics in `internal/contracts` and exercised them only through in-process ports: the
deterministic simulators and the worker's test port. The
[M1 contract freeze](../architecture/contract-freeze-30.md) records "in-process adapters only; no
production transport qualification".

[ADR-0002](0002-initial-application-boundary.md) keeps providers as external systems that never write
the product database and requires any new runtime boundary to define its protocol, authentication,
idempotency, cancellation and failure reconciliation.
[ADR-0006](0006-sql-outbox-durability-decision.md) requires a lost reply to stay uncertain and be
reconciled rather than resent. [ADR-0007](0007-surface-api-transport-and-authentication.md) keeps JSON
Schema as the only schema language and an operator-configured OpenID Connect provider as the trust
root for callers.

Three things are therefore undecided: where adapters run in production, how frames cross the
boundary, and how an adapter proves which provider it speaks for. `contracts.Peer` is documented as
"established by transport authentication, not by anything the provider writes in a frame", but no
transport establishes it yet.

The Plane-Bot evidence
([deep dive 03](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-03.md) §1–2 and §5,
[deep dive 01](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-01.md) §3–4) shows a
shipped system that keeps every provider out of the authority's process, even on one machine: a
coordinator utility process, a long-running execution daemon and a separate computer-use helper. The
user's machine opens its own connection to the backend and receives request frames streamed over it;
cloud pods run the same daemon protocol and are reached with a per-resource token. One daemon protocol
serves both placements. Its transport is protobuf, and its components trust one vendor; neither is
adopted. The historical [specification](../../architecture-explore/docs/spec/SPEC.md) requires
workers and adapters to authenticate as scoped services with credentials bound to tenant, attempt,
lease epoch and expiry, rotated and revoked, and notes that a runtime adapter can be a worker-side
library.

## Decision

### Placement

Production adapters always run out of process. An adapter is a separate program, usually deployed
next to the provider it fronts, that speaks ControlFrame to the control plane on one side and the
provider's native interface on the other. The control-plane binary contains no production provider
adapter.

In-process ports remain for the deterministic simulators and tests. They may not be enabled by a
production configuration; the deployment ADR decides how a production build enforces that.

### Connection direction and transport

The adapter always opens the connection. It dials the control plane over HTTPS and upgrades to one
WebSocket connection. The control plane never dials an adapter. This one model serves an adapter
behind a firewall or NAT and an adapter running beside a cloud provider, without inbound reachability
to provider sites.

Frames are JSON. Each WebSocket text message carries exactly one ControlFrame that validates against
the adapter JSON Schemas under `contracts/jsonschema/`. Binary messages, batched frames and frames
above an operator-configured size limit are malformed and end the connection with a typed failure.
The WebSocket subprotocol names the framing (`dac-plane.controlframe.v1`); the protocol version and
capabilities are still negotiated by lifecycle frames as ADR-0003 requires, and nothing is processed
before negotiation selects them.

Over one connection both directions carry frames. The control plane sends request and cancel frames
and receives reply frames; the adapter sends event frames for the streams the control plane
subscribed to. A reply is matched only to a request sent on the same connection.

### Failure and reconciliation

The transport adds no outcome semantics. A connection that closes, times out or fails liveness checks
after a request was sent, but before its reply arrived, returns a transport error for that request.
The request becomes uncertain and is reconciled from bound observations or by replaying the same
stable operation where the protocol allows it; it is never resent as a new operation. A provider with
no live connection has no port, and its outbox rows stay pending, exactly as for an unregistered
provider today. Cancellation remains an intent until a stop is observed.

The connection uses WebSocket ping frames for liveness. Event streams resume from their recorded
cursors after reconnection; a cursor the adapter can no longer serve produces the cursor-too-old and
rehydration path ADR-0003 defines.

### Several connections and several replicas

A provider registration may hold several live connections, for example several adapter instances or
connections to several control-plane replicas. Any live connection may carry a new request. A
control-plane replica claims dispatch work for a provider only while it holds a live connection for
that provider, so a claimed row is always sendable from the claiming replica. Claims stay fenced, so a
replica that loses its connection after claiming settles nothing it did not observe. How replicas are
deployed is decided in the deployment ADR; the fencing behavior is proven in the operations test issue.

### Adapter authentication

An adapter authenticates when it opens the connection, with an OAuth 2.0 client-credentials access
token from an identity provider the operator configured, sent as a bearer token on the upgrade
request. Token verification follows ADR-0007: configured issuer, allowed algorithm, audience naming the
control plane's adapter endpoint, expiry, not-before and a maximum lifetime.

The pair (issuer, subject) maps to a provider registration only through an explicit stored binding of
(issuer, subject) to (tenant, provider). That binding is the only source of `contracts.Peer`. Nothing
an adapter writes in a frame can change the tenant or provider it speaks for. Unbound identities are
refused before negotiation. Providers are audiences of authority, not principals, so an adapter
registration grants no Surface API access. Registrations and bindings are created only through the
trusted provisioning command ADR-0007 defines.

A connection lives no longer than its token. Before the token expires the control plane stops sending
new requests on that connection, and at expiry it closes it; the adapter reconnects with a fresh token.
Requests still in flight at close follow the reconciliation rules above. Disabling a binding closes
its live connections.

Connection authentication says which provider is speaking. It does not authorize any action:
per-operation authority remains the scoped `AuthorityGrant`, bound to tenant, Attempt, audience,
policy generation and expiry, and event binding remains `BindEvent` against the recorded provider
operation.

### Adapter kit

Publish an adapter kit as a separate Go module in this repository, under `adapterkit/` with its own
`go.mod` and version tags. It contains what an adapter author needs: the connection client (dial,
token source, negotiation, liveness, reconnection), the wire codec for ControlFrame, a provider-side
handler interface, and the adapter conformance suite. The kit never imports a package of the
control-plane module, so adapters do not depend on control-plane internals or versions. Lifecycle
packages stay under `internal/`.

The JSON Schemas remain the language-neutral contract. Both `internal/contracts` and the kit are
tested against them, so an adapter written in another language is held to the same contract by the
same schemas and the conformance suite, which runs against any adapter endpoint. The deterministic
simulators become reference adapters built on the kit and run out of process in the integration
suites.

The kit is versioned separately from the control plane. A kit major version corresponds to a
ControlFrame protocol major version; a control plane supports every protocol version it advertises
in negotiation.

### Audit and secrets

Connection opens, refusals, negotiations and closes are logged with the provider registration and a
reason category. Tokens, upgrade headers and provider credentials are never written to a log, audit
record, durable event, frame payload or error body.

## Alternatives considered

- **In-process adapters for first-party providers:** rejected. It makes provider code part of the
  control plane's failure and credential domain, which ADR-0002's split triggers name as reasons for a
  boundary, and it leaves the wire path unproven for the providers that matter most.
- **Control plane dials adapters,** as Grok Bot does for cloud pods: rejected. It needs inbound
  reachability to every provider site and a second connection model to secure and test. An adapter
  beside a cloud provider can dial out just as easily.
- **Both connection directions:** rejected for the same reason; one model is enough for both
  placements.
- **gRPC or ConnectRPC streams with protobuf,** as in the Plane-Bot transport: rejected because it adds
  a second schema language beside JSON Schema, contrary to ADR-0001 and ADR-0007.
- **HTTP request/response plus Server-Sent Events:** rejected because ControlFrame is bidirectional
  and requests flow from the control plane to an adapter that opened the connection; two channels
  would split reply correlation across connections.
- **Stdio or local-socket transports:** not adopted. Grok Bot's computer-use helper uses stdio, but a
  child-process transport only works on one host and would be a second transport to qualify.
- **Mutual TLS:** not adopted for the first version. It avoids bearer tokens but requires every
  operator to run a PKI; client credentials reuse the identity provider ADR-0007 already requires.
- **Control-plane-minted connection tokens,** as Grok Bot mints per-resource tokens: rejected because
  the control plane would hold and rotate credential material.
- **A public package inside the control-plane module:** rejected because adapters would depend on the
  whole control-plane module and its version.
- **Schemas and conformance suite only, no kit:** rejected because every Go adapter would re-implement
  negotiation, liveness and reconnection, where subtle mistakes break reconciliation.

## Consequences

- #72 implements the WebSocket endpoint, the connection registry that supplies a `contracts.Port` per
  connected provider, the provider registration and binding records with JSON Schemas and agreement
  tests, the kit module, the conformance suite, and the out-of-process simulators. The worker's fixed
  provider-to-port map becomes that registry.
- The full M1 recovery, security and migration suites must pass with simulators out of process,
  including lost, duplicated and malformed frames and connections closed mid-request.
- Token expiry closes connections routinely; that path is a normal reconnection, not an incident, and
  must be tested as such.
- #68 decides TLS termination for the adapter endpoint, how a production build excludes in-process
  ports, and how replicas expose the endpoint. #74 proves claim fencing when connections move
  between replicas.
- Operating dac-plane requires the identity provider to issue client credentials for each adapter
  registration.
- No production provider is selected; #75 qualifies the first ones against the conformance suite.
