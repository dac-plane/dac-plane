# Implementation plan: issue #17 versioned provider adapter contracts

Tracker: [GitHub issue #17](https://github.com/AssahBismarkabah/dac-plane/issues/17) (epic #11)

## Objective

Define the provider-neutral adapter protocol around the product records from #13 to #16: the
versioned `ControlFrame` with handshake, requests, cancellation and typed failures; the
replayable `EventCursor` stream; authenticated binding of provider events before any lifecycle
mutation; synchronous `HookRequest` / `HookResponse` interception; `ActionPrecondition` with
per-resource serialization; and the port catalogue for runtime, environment, enforcement and
artifact-store roles. Pure Go plus JSON Schema 2020-12, in `internal/contracts`. No transport,
SDK or provider selection.

## Sources

- ADR-0003 sections "Versioned control protocol", "Replayable observation stream", "Action
  interception" and "Stateful action preconditions" (the normative text).
- Review invariants: provider events are authenticated and bound to tenant, Attempt, provider
  operation and sequence before they affect product state; peers select one version and
  capability set before work, with no implicit downgrade; cursor acknowledgement is ingestion
  progress, not lifecycle completion; governed effects need an affirmative pre-action hook before
  the deadline; audit uses digests, never raw payloads.
- Container view: runtime, environment and enforcement ports; "authenticated normalized-event
  ingestion".
- Plane-Bot evidence (shape only, no vendor names): control port v3 frames and failure taxonomy
  (DEEP-DIVES-03 §1), transcript watch frames with generations, tombstones and rehydration
  (DEEP-DIVES-04 §1), hook kinds and response capabilities (DEEP-DIVES-04 §2), staleness tokens
  and one-action-at-a-time discipline (DEEP-DIVES-03 §4).

## Design (`internal/contracts`)

- **Handshake.** A provider sends `lifecycle/hello` with supported versions, offered and
  required capabilities. `Negotiate` selects the highest common version and the capability
  intersection, failing with `protocol_mismatch` or `unsupported_capability`; there is no
  downgrade. A `Gate` admits frames in order: anything before a valid hello, any request or
  event before `ready`, or any malformed frame shuts the session down with a typed failure
  before provider work starts.
- **Frames.** `Frame` is a tagged union of `lifecycle | request | cancel | reply | event`.
  Requests carry a stable operation identity (idempotency across redelivery) and a fresh
  delivery identity, tenant and Attempt binding, method, bounded arguments, `person |
  background` demand and a deadline. Replies are `ok | failed` with a typed `Failure`.
- **Failures.** A closed code set: protocol mismatch, unsupported capability, malformed frame,
  unknown operation, invalid arguments, unauthenticated, unauthorized, cancelled, deadline
  exceeded, transport unavailable, provider failed.
- **Event stream.** `Cursor` binds tenant, Attempt, operation, stream and generation and records
  `after_updated_sequence`. Stream frames are `connected | rows | cleared | cursor_too_old |
  heartbeat`; rows carry entries and tombstones, a replay flag, and payloads that are inline
  (hash verified) or omitted (hash only). `Ingest` advances the cursor: entries at or below the
  cursor are counted as duplicates, out-of-order entries within a frame are malformed, cleared
  generations and cursor-too-old require rehydration.
- **Binding.** `BindEvent` authenticates a provider event against the transport-authenticated
  peer and the Attempt's recorded provider operation, returning a `BoundEvent` whose fields are
  unexported, so lifecycle mutation can only take an event that went through binding.
- **Hooks.** `HookRequest` (pre-action, post-action, action-failure) and `HookResponse`
  (permission, rewritten input, user and agent messages, additional context, usage).
  `DecidePreAction` fails closed on a missing, late, malformed or denying response and
  otherwise yields the effective input and its digest, from which the effective
  `identity.ActionRequest` is built. `HookEvidence` keeps digests only.
- **Preconditions.** `ActionPrecondition` binds an action to an observation token; a stale token
  is refused. `RequireSerial` refuses a second in-flight stateful action on the same resource.
- **Ports.** `Role` (`runtime | environment | enforcement | artifact_store`), its method
  catalogue, and a transport-neutral `Port` interface; a request for a method outside its role
  is `unknown_operation`.

Schemas go in `contracts/jsonschema/adapter/`; the agreement tests are named
`TestContractConformance*` (the issue's verification command), and both contract guards cover
the new package. A new guard rejects vendor identifiers anywhere under `contracts/`.

## Ordered tasks

1. Identifiers (Delivery), failures, frames and the handshake gate.
2. Cursor, stream frames and ingestion with payload hash verification.
3. Provider event binding.
4. Hooks, preconditions and the port catalogue.
5. Schemas, conformance tests, guard extensions (package list, vendor identifiers).
6. Mutation checks and the full CI set.

## Verification

```sh
go test ./internal/contracts/...
go test ./... -run ContractConformance
go test -race ./...
go test -count=1 -tags=integration ./...
```

## Boundaries

- No network transport, wire encoding beyond JSON, provider SDK or credential ceremony.
- JSON canonicalization is not defined: digests cover the exact bytes delivered.
- Applying bound events and hook decisions to records is #23 to #25.
