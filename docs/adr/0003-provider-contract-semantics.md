# ADR-0003: Adopt provider-neutral execution contract semantics

- Status: Accepted
- Date: 2026-10-15

## Context

The initial baseline establishes one product-owned lifecycle with replaceable runtime, environment,
enforcement and artifact providers. It does not yet define enough behavior at those boundaries for the
domain and adapter contracts to be implemented without returning to exploration material.

## Observed evidence

The static Plane-Bot investigation provides evidence from a shipped system for versioned control
frames, replayable event streams, runtime and environment correlation, scoped action authority,
interception hooks, spend authorization, fenced artifact admission and stateful computer actions. The
relevant observations are recorded in the
[master record](../../architecture-explore/plane-bot-explore/notes/MASTER-RECORD.md),
[dac-plane mapping](../../architecture-explore/plane-bot-explore/notes/DAC-PLANE-MAPPING.md),
[control-port deep dive](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-03.md) and
[lifecycle and hook deep dive](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-04.md).
These sources describe a vendor-owned system and are evidence, not active requirements.

## Decision

Adopt the following provider-neutral semantics. The active Go and JSON Schema contracts may refine
field names and representations, but they must preserve these behaviors and invariants.

### Versioned control protocol

`ControlFrame` is the logical adapter control protocol across an external process boundary. It has five
frame families:

- lifecycle frames establish readiness or shutdown and carry a protocol version;
- request frames carry a stable request identity, operation, arguments and optional demand class;
- cancel frames target an in-flight request identity;
- reply frames return a typed success or failure for that request identity; and
- event frames carry unsolicited, authenticated provider observations.

A peer advertises its supported protocol versions and capabilities before accepting work. The other
peer selects one mutually supported version and capability set, and the ready frame acknowledges both;
no request or event may be processed before that selection. No common version, any unavailable required
capability, a malformed frame or an invalid handshake produces a typed protocol failure and shuts down
the interaction before provider work begins. There is no implicit downgrade. Failure families must
distinguish protocol mismatch, unsupported capability, malformed input, unknown operation, invalid
arguments, unauthenticated or unauthorized use, cancellation, deadline expiry, transport
unavailability and provider failure.

The demand classes are `person` and `background`. They are scheduling hints: person-facing work may be
preferred, but demand cannot change authorization, budget, permission-ceiling or lifecycle rules.

### Replayable observation stream

`EventCursor` binds stream progress to a tenant, Attempt, provider operation, stream and generation.
Within a generation, `sequence` identifies a logical entry and `updatedSequence` orders entry revisions
and tombstones. The event contract represents connected state, row batches, replay markers, heartbeats,
cleared generations, cursor-too-old responses, rehydration, tombstones, payload hashes and omitted
payloads.

A cursor acknowledgement records how far authenticated observations were ingested. It is not proof
that a provider effect occurred or that a product lifecycle transition completed. Cleared generations
and cursor-too-old responses require explicit rehydration. Unknown send durability is a first-class
indeterminate command outcome, not an ordinary failure. It enters reconciliation and is never treated
as proof that no effect occurred or as permission to redispatch.

The optional
[CloudEvents sequence extension](https://github.com/cloudevents/spec/blob/main/cloudevents/extensions/sequence.md)
does not replace `EventCursor`: it orders values within one event source but does not define
generations, revisions, stream completeness, tombstones or rehydration.

### Attempt execution bindings

Runtime sessions, runtime turns, environment resources and provider operations are correlated to one
Attempt through explicit bindings. They remain provider-native resources, not product Tasks or
alternate lifecycle authorities. Typed turn failures and partial or unknown resource observations stay
explicit so the product can reconcile them without inventing provider success or failure.

### Environment and harness lifecycle

Environment observations distinguish absent, starting, running and hibernated resources. They report
provider state without transferring lifecycle authority away from the product. Maintenance work uses
expiring, fenced claims; a stale claimant cannot complete work after takeover.

Harness qualification uses `off`, `shadow` and `live` modes. Shadow execution may record comparison
evidence, but it cannot mutate authoritative lifecycle state or perform governed external effects.
Migration passes, holds and preview/execute administration are durable, authorized and auditable. A
preview cannot have execution effects, and executing a migration requires a distinct authorized
command.

### Scoped action authority

`AuthorityGrant` binds authority to its tenant, Attempt, provider operation, action class and applicable
scope. Scope classes are global, setting, subject and call. The authority model distinguishes one-time
allow, standing allow and deny/never policy:

- after interception determines the effective action, the dispatch transaction revalidates authority
  and atomically consumes and retires one-time authority when applicable;
- standing authority remains valid only for its exact scope until expiry, revocation, scope change or
  policy-generation invalidation; and
- deny/never policy blocks issuance and use of conflicting authority.

A permission ceiling is authoritative: neither a local setting nor a provider may grant above it.
Policy changes advance a generation so stale cached authority fails closed. The dispatch transaction
revalidates the effective action's authority, scope, expiry, revocation state, policy generation and
permission ceiling. It then atomically consumes one-time authority when applicable and persists
`AuthorityUse`, the audit and durable event records, the stable provider operation and its outbox entry.
A hook denial or failed authority predicate atomically persists an `AuthorityUse` denial bound to the
stable requested-operation identity plus safe audit and durable event evidence, but creates no
executable provider operation or dispatch outbox entry. Successful provider execution returns an
operation-bound receipt. Provider execution uses the stable operation identity for idempotency and
reconciliation. An indeterminate external outcome never restores consumed one-time authority.

### Action interception

`HookRequest` and `HookResponse` define synchronous interception around governed actions. A pre-action
hook may allow, deny or rewrite input and may return user messages, agent messages and additional
context. The required order is: evaluate the pre-action hook without external effect; determine the
effective action; transactionally revalidate authority and persist its use together with the provider
operation and durable dispatch/outbox records; then dispatch provider execution.

A governed effect requires a valid affirmative response before the deadline. Denial, timeout, hook
failure, malformed response or transport failure before durable dispatch produces no executable
operation and no external effect. Loss or failure after dispatch makes the provider outcome
indeterminate and enters reconciliation instead of restoring authority or repeating the effect.

Authorization is evaluated against the effective rewritten action. Audit and evidence records use
classified, redacted projections or cryptographic digests of original and effective inputs; provider
credentials, secret-bearing fields and unrestricted raw payloads never enter product or audit records.

Post-action and failure hooks record safe evidence references or redacted projections, typed outcome,
duration and usage accounting. Hooks enforce action policy and collect evidence; they never own product
lifecycle transitions.

### Spend authorization

`SpendChallenge` defines an abstract two-phase flow: begin a challenge, assert authorization and persist
a durable receipt. Challenges and receipts bind tenant, principal, Task, Attempt, provider operation,
amount, unit or currency context, expiry and replay identity. Expired, replayed or mismatched
assertions fail.

Spend authorization does not reserve or consume Budget. Product-owned integer reservation,
consumption and ceiling enforcement remain separate and authoritative. The cryptographic ceremony and
credential provider remain open.

### Fenced artifact admission

`ArtifactCommitOutcome` has three values: `committed`, `retry` and `fenced`. Only a committed manifest
with a verified digest may create a registered Artifact or be referenced by a ResultRevision. Retry
records no admission and may repeat under the stable operation identity. Fenced rejects a stale writer
without changing the winning manifest.

### Stateful action preconditions

`ActionPrecondition` binds a stateful action to the observation or snapshot on which it was planned. A
stale token is rejected and requires re-observation. Stateful actions are serialized per controlled
resource: parallel reasoning does not authorize parallel physical actions.

## Rejected implementation choices

The decision adopts behavior, not Plane-Bot's implementation. In particular, it rejects:

- the vendor-owned trust topology and provider-specific identifiers;
- the 485-method API surface as the shape of the dac-plane Surface API;
- ConnectRPC as a selected transport;
- Temporal as a selected workflow or runtime implementation;
- WebAuthn as the selected spend-authorization ceremony;
- native helpers, proprietary binaries and provider-specific state names as requirements;
- provider writes to authoritative product records; and
- simulator parity as evidence that a production provider is qualified.

## Consequences

- Issues #13 through #17 must express these semantics in transport-neutral domain, identity and adapter
  contracts plus JSON Schema 2020-12 before persistence begins; issue #18 verifies their agreement.
- Issues #19 through #21 must preserve the corresponding cursor, authority, claim, shadow, artifact and
  reconciliation invariants in storage and dispatch.
- Issue #22 must implement the complete contract family in deterministic simulators. Issues #23 through
  #26 must exercise it only through application command and event boundaries.
- Issues #27 and #28 must prove recovery and security behavior under replay, stale state, lost replies,
  claim takeover and fenced operations. Issue #29 evaluates whether SQL/outbox remains sufficient for
  those guarantees, and issue #30 freezes only the contracts supported by evidence.
- M1 targets full contract-and-simulator parity. Runtime, environment, enforcement, credential ceremony,
  artifact-store and computer-use integrations remain separate production qualification work.
- The responsibility and container diagrams do not change because lifecycle ownership and running
  software boundaries are unchanged.
