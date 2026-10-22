# Issue #67 — Decide the provider adapter process boundary and wire transport

Tracker: https://github.com/dac-plane/dac-plane/issues/67
Branch: `docs/67-adapter-boundary-adr`, from `main` at `5b84977`.
Parent: #65 (M2: Deployable control plane).

## Direction

Record ADR-0008 as a decision, not an implementation. It decides where runtime, environment,
enforcement and artifact-store adapters run, how ControlFrame crosses the process boundary, how an
adapter authenticates, and what the public adapter kit is. Inputs already decided are reused:

- ADR-0002: providers are external systems behind ports and never write the product database; a
  runtime split needs an ADR covering protocol, authentication, idempotency, cancellation and
  failure reconciliation.
- ADR-0003: ControlFrame is the logical protocol across an external process boundary, with
  fail-fast negotiation and no implicit downgrade.
- ADR-0006: lost replies stay uncertain and are reconciled, never resent as if no effect occurred.
- ADR-0007: JSON Schema is the only schema language; external OpenID Connect is the trust root.

Decisions confirmed with the maintainer on 2026-10-22, after reviewing the Plane-Bot evidence:

- Production adapters always run out of process. In-process ports remain for the deterministic
  simulators and tests only.
- The adapter always opens the connection: one WebSocket over HTTPS carrying JSON ControlFrames.
- An adapter authenticates with OpenID Connect client-credentials tokens from the operator's
  identity provider, bound to a provisioned provider registration. Per-operation authority stays in
  `AuthorityGrant`.
- The adapter kit is a separate Go module in this repository.

Evidence used: `architecture-explore/plane-bot-explore/notes/DEEP-DIVES-03.md` §1–2 and §5 items 4–6
(versioned control port; machine receives server-streamed request frames over its own outbound
connection; cloud pods dialed with a per-resource token; one daemon protocol in two placements;
Computer Use as a separate process), `DEEP-DIVES-01.md` §3–4 (separate daemon and coordinator
processes), and `architecture-explore/docs/spec/SPEC.md` (workload identity row; "the runtime
adapter can be a worker-side library").

## Implementation

- [x] Confirm the four open decisions before drafting.
- [x] Read the adapter contracts (`internal/contracts/port.go`, `frame.go`, `gate.go`, `stream.go`)
  and how the worker holds ports (`internal/worker/worker.go`).
- [x] Draft `docs/adr/0008-adapter-process-boundary-and-wire-transport.md` with status Proposed.
- [ ] Maintainer review of the draft.
- [ ] On approval: set Accepted and link it from `docs/architecture/README.md` in the same PR.

## Verification

- `git diff --check`
- Every relative link in the ADR resolves to a file on this branch.

## Findings carried to implementation issues

- The worker takes a fixed `map[ids.ProviderID]contracts.Port` at construction. Adapters that
  connect and disconnect need a connection registry that satisfies `contracts.Port` per provider and
  reports "no port" while disconnected, so rows stay pending as they do today (#72).
- With several control-plane replicas, an adapter connection terminates on one replica. Work for a
  provider must be claimed only where a connection exists (#72, proven in #74; deployment shape in
  #68).
- `contracts.Peer` is documented as established by transport authentication; nothing establishes it
  yet. The provider registration and its (issuer, subject) binding are new records (#72).
- Simulators must run behind the kit out of process so the full M1 suites run over the wire (#72).

## Remaining boundaries

This issue selects no production provider (#75), no deployment or TLS placement (#68), and no
adapter-side sandboxing of the provider itself. Stdio or local-socket transports and control-plane
dial-out are not adopted.
