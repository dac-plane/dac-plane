# Issue #25: governed actions

Tracker: [#25](https://github.com/AssahBismarkabah/dac-plane/issues/25). Branch:
`feat/25-governed-actions`, based on #55 at `7bb523a`. One PR carries this feature and its review fixes.

## Contract and architecture

Implement ADR-0003 scoped action authority, synchronous interception, operation-bound
receipts, and serialized stateful actions. The active architecture and ADRs are authoritative.
The Plane-Bot `DEEP-DIVES-03.md` sections 2–4 supply evidence for source attribution,
retirement, scope classes, generation invalidation, and observe/act discipline;
`DEEP-DIVES-04.md` supplies hook semantics. Exploration files remain read-only.

Existing pure authority and hook contracts are reused. One-time and standing approvals
retain Decision provenance. An approval names a distinct action operation, while the runtime
turn continues to carry its attention event and receive its steer/cancel. Runtime operation
lookups exclude governed actions; action rejection and receipt reconciliation do not resolve
the runtime's independent lifecycle.

## Ordered implementation

1. Extend grants and uses with Decision provenance and exact scope on uses. Add
   `approve_standing`, scope on approval requests, `ActionExecution`, and
   `EnforcementReceipt`; mirror the changes in JSON Schema and agreement tests.
2. Add migration 00005 without editing released migrations. Denial evidence may exist
   without an executable provider operation. `governed_action` owns immutable replay
   identity; allowed uses additionally create a provider operation and dispatch outbox.
   Receipts are immutable and reference both the requested and executable operation.
3. `EnforceGovernedAction` authenticates the provider, evaluates a bounded pre-action
   hook outside the database transaction, then locks operation replay identity, policy,
   Attempt, and grant. Authorization checks the effective input. One transaction records
   hook evidence, authority use, audit/event evidence, retirement, and allowed dispatch.
   Failure records a denial with no executable operation or outbox. Changed operation
   payloads conflict; replay returns the original evidence even after expiry/cancellation.
4. The worker rechecks current grant, policy, active Attempt, harness, deadline and
   leases before recording its send marker. An existing marker after a crash becomes
   uncertain; it is never resent. Execution is bounded by configured send timeout and
   action deadline. Receipt insertion, operation completion and fenced acknowledgement
   commit together. Only an exact authenticated receipt settles an uncertain action.
5. Extend the deterministic enforcement simulator with separate hook/execution dedupe,
   immutable execution fingerprints, resource reservations and receipt replay. Check the
   observation token under the resource mutex immediately before mutation and capture
   the resulting state digest in that same critical section.
6. Exercise the full flow through application commands/events and the worker. Run the
   focused acceptance suite, contract guards, integration/race checks, then `just ci`.
   Review correctness, architecture, security and recovery before committing.

## M1 execution boundary

The implemented action is a fixed `resource.mutate` simulator operation. Its input is a
bounded JSON object with `resource` and `value`; scope is the named resource subject.
The hook may rewrite the value. A rewrite changing the resource is refused rather than
reusing the original subject's authority. The executor retains input by digest from the
pre-action stage; product records/outbox contain only identifiers, scope, digests and the
observation precondition. No raw values or credentials enter PostgreSQL.

`WithEnforcementPorts` explicitly binds the enforcement role of the same qualified
provider audience alongside its runtime role. This implements the baseline's runtime-hook
enforcement option; a separate gateway identity requires future explicit audience mapping.
No production policy engine, tool catalogue, secret broker or payload store is selected.

Existing persisted grants can lack Decision provenance, but this execution boundary
refuses them. New application approvals always carry their recorded Decision. Historical
scope-less approval records cannot issue new authority through the updated command.

## Acceptance evidence

`TestGovernedAction*` covers exact one-time execution and retirement, replay and changed
payload conflicts, standing grants, requested/effective digests, revocation, generation and
ceiling denials, expired/missing/mismatched grants, hook denial/missing/timeout/malformed/
transport failure, stale tokens, per-resource serialization, changed state at the effect
boundary, runtime-operation collisions, queued-action invalidation, and lost-reply receipt
reconciliation without repeating effects. Additional tests cover forbidden scoped issuance,
contract conformance, migration up/down, and existing runtime/attention/cancellation flows.

```sh
just test-contracts
just test-integration ./internal/application/ -run GovernedAction
just test-integration-race ./internal/application/ -run GovernedAction
just ci
```

macOS integration commands use the SDK selection in `justfile`. An empty test-name match
is not acceptance evidence. Verification results are reported only after observed completion.

## Completed verification

Implementation and review fixes are complete. `just test-contracts`, focused governed-action
integration tests (normal and race-enabled), and the full `just ci` suite passed. The final
CI-equivalent run used `SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" just ci` on macOS;
the race-enabled integration packages passed, including application (223.530s), worker
(55.073s), PostgreSQL (99.638s), and migrations (27.773s). Vulnerability scanning reported
no vulnerabilities and workflow linting passed.

The acceptance suite additionally verifies exact-byte JSON digests and grant issue,
retirement and revocation evidence. A temporary mutation that inverted hook refusal made
the denial acceptance test fail; the original gate was restored and the final suite passed.
No production provider, network transport, or deployed environment was exercised.
