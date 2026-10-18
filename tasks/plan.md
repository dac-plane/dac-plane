# Implementation plan: issue #22 deterministic provider simulators

Tracker: [GitHub issue #22](https://github.com/AssahBismarkabah/dac-plane/issues/22) (epic #11)

## Objective

Deliver deterministic runtime, environment, enforcement and artifact-store
implementations behind the real `contracts.Port`, driven by explicit scenarios,
pausable at every external-effect boundary, with effect ledgers that prove
retries never duplicate external work. Depends on #17 and the approved #18
contract gate (both merged). The issue's verification is
`go test ./internal/simulator/...` and `go test -race ./internal/simulator/...`.

## Sources

- `docs/architecture/README.md` (line 114): the simulator must expose the same
  adapter contract, attention behavior, cancellation acknowledgement and event
  replay as a production runtime; the suite must exercise protocol negotiation
  and failure, cursor replay and rehydration, authority retirement and
  revocation, hook rewriting and denial, spend challenge replay, environment
  and shadow behavior, artifact fencing and stale action preconditions.
- ADR-0003 provider-contract semantics: Operation is the provider's idempotency
  key (redelivery keeps it, gets a new Delivery); a transport that loses the
  reply after delivery returns an error, never a guessed outcome; cancellation
  is confirmed by observation; unknown durability enters reconciliation; shadow
  harnesses never mutate authoritative state.
- `internal/contracts` (`port.go` Port/Stream; `frame.go`
  Request/Reply/Cancel/Lifecycle/Negotiate; `stream.go`
  StreamFrame/Event/BindEvent/Ingest/Cursor; `hook.go`
  HookRequest/HookResponse/DecidePreAction/ActionPrecondition/CheckPrecondition/
  RequireSerial).
- `internal/lifecycle` pure machines the scenarios compose:
  `execution.go` operation machine, `observation.go` EnvironmentObservation,
  `artifact.go` StoredObject/AdmitArtifact, `spend.go` challenge/assertion/
  receipt, `claim.go` and `harness.go` migration, `identity.Authorize`.
- `internal/worker/port.go` loopback Port: precedent for scripted in-process
  providers, and its comment fixes the boundary — "simulator parity is #22's
  job, and a fake provider in production code would silently satisfy dispatches
  with no effects."
- `architecture-explore/` evidence only: Plane-Bot dedupe-by-operation rules,
  cursor/tombstone/heartbeat stream semantics, hook rewrite/denial, artifact
  fences (MASTER-RECORD, DAC-PLANE-MAPPING, DEEP-DIVES-03/04); SPEC §28
  fault-injection precedent for pause points.

## Design

- New package `internal/simulator/`, pure and store-free: `simulator.go`
  (Config, Boundary, Control/Pause, Ledger/Effect, Reply, sentinel errors),
  `streams.go` (StreamScript + hash-computing frame builders), `runtime.go`,
  `environment.go`, `enforcement.go`, `artifact.go`, `spend.go` (Spender),
  `resource.go` (stateful tool), `compare.go` (primary vs shadow effect
  parity). All role types implement `contracts.Port` and expose `Hello()`,
  `Control()` and `Ledger()`.
- Scenario or bust: every simulator is constructed from an explicit,
  immutable scenario; an operation without a script fails loudly
  (`ErrUnscripted`), never invents a success. Redelivery of an effectful
  operation reuses the recorded reply (stamped with the new Delivery) and
  appends no second effect — the ledger's Deliveries/Effects split is the
  retry-duplication evidence. No-effect operations answer each delivery fresh.
- Pause/resume: `Control.Pause(op, boundary)` with `BeforeEffect`,
  `AfterEffect` and `BeforeFrame`; registration before the operation runs,
  `Reached()`/`Release()`, no sleeps. A call cancelled while paused leaves an
  applied-but-unknown effect for the redelivery to dedupe.
- Streams are scripts: builders compute payload hashes; the suite drives real
  `BindEvent`/`Ingest` for duplicate, reordered, replay, tombstone, cleared,
  cursor-too-old, heartbeat and malformed frames. Simulators emit the events
  they claim through `Event` — forgeries are the consumer's refusal to prove.
- Product-side criteria are proven as compositions: the simulator is the
  external party, the real contract/domain code decides (identity.Authorize,
  lifecycle machines, AdmitArtifact, Ingest, DecidePreAction), the ledger
  proves effect or none. Machines already covered by #14/#15/#17 fixtures are
  not restated.
- `internal/bootstrap` keeps an empty registry; the simulators are test-facing
  doubles, never production wiring.

## Ordered tasks

1. Rewrite this plan (done, committed separately).
2. PR-22a (`internal/simulator` core + runtime + streams): `simulator.go`,
   `streams.go`, `runtime.go` with tests for pause/resume, ledger splits,
   scripted replies, redelivery dedupe, lost acknowledgements, cancellation,
   negotiation and failure, malformed frames, binding forgeries, stream
   classification through `Ingest`, demand ordering.
3. PR-22b (environment, enforcement, spend): `environment.go`,
   `enforcement.go`, `spend.go` with tests for provision/stop effects,
   observation sequences and stop confirmation, separate runtime and
   environment termination, hook allow/deny/rewrite/usage through
   `DecidePreAction`, authority retirement and revocation, spend replay
   refusal.
4. PR-22c (artifact, resource, migration, parity, trace): `artifact.go`,
   `resource.go`, `compare.go`, plus migration/claim scenario tests and a
   flagship all-roles lifecycle trace run twice for determinism; committed/retry/
   fenced outcomes, stale-token and serialization refusal, holds and
   preview/execute, shadow comparison.
5. Full gate per PR: `gofmt -l cmd internal`, `go vet ./...`,
   `go vet -tags=integration ./...`, `go test ./...`,
   `go test -race ./internal/simulator/...`, `just test-contracts`,
   `git diff --check`.

## Verification

```sh
gofmt -l cmd internal
go vet ./...
go vet -tags=integration ./...
go test ./...
go test -race ./internal/simulator/...
just test-contracts
just ci
git diff --check
```

## Boundaries

- No changes to `internal/worker`, `internal/bootstrap`, `internal/platform`,
  `contracts/jsonschema` or the schema coverage guard lists. A scenario that
  exposes a real bug is fixed in its own commit with its own tests.
- No integration-tagged tests in `internal/simulator`; store/worker end-to-end
  proofs stay with #21c and #27.
- No production wiring of simulators, no network emulation, no model quality
  testing, no production provider compatibility.
- No baseline, diagram, or accepted-ADR edits; `architecture-explore/`
  read-only. No new ADR: ADR-0003 already fixes the semantics.
