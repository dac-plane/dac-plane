# ADR-0012: Negotiate stable-operation replay before crash takeover redelivery

- Status: Accepted
- Date: 2026-10-26
- Issues: [#74](https://github.com/dac-plane/dac-plane/issues/74),
  [#88](https://github.com/dac-plane/dac-plane/issues/88)
- Refines: the ordinary-send recovery rule in
  [ADR-0006](0006-sql-outbox-durability-decision.md) and
  [ADR-0008](0008-adapter-process-boundary-and-wire-transport.md)

## Context

The deployed process-kill scenario pauses the deterministic runtime before its effect, kills the
claiming control-plane process, expires only that operation's outbox and Attempt leases, and lets a
second replica take over. The first delivery has already crossed the process boundary and the worker
has committed `send_started`; PostgreSQL therefore cannot prove whether the provider acted. The
existing blanket recovery path resolves every such takeover as uncertain and never redelivers. In
the observed before-effect case that preserved safety but prevented progress: the simulator recorded
one delivery and zero effects.

ADR-0006 already permits replay of the same stable operation where the provider protocol explicitly
allows it. ADR-0008 requires adapters to use the operation identity as their idempotency key, but the
wire session did not negotiate whether a particular adapter actually provides that stronger
guarantee. Treating every adapter as replay-safe would turn an implementation comment into an
unverified external-effect assumption.

## Decision

Add the optional ControlFrame capability `operation.replay`. Both peers must offer it and the ready
frame must select it before the control plane treats the connected registration as a stable-operation
replayer.

For a selected session, an ordinary operation whose committed send marker survives claim and Attempt
lease expiry may be delivered again with the same operation identity, Attempt, method, arguments and
deadline and a fresh delivery identity. The adapter must serialize deliveries by operation identity,
apply the external effect at most once, and return the recorded result when a prior delivery already
finished. The replacement worker still runs the normal cancellation, deadline, harness, budget and
Attempt-lease checks. Claim and Attempt epochs fence the prior worker from settlement.

If no live connection selected `operation.replay`, the existing behavior remains: a takeover does not
resend and moves the operation to uncertain reconciliation. If a capable connection disappears after
takeover preparation but before replay, the worker releases the claim while retaining `send_started`;
it never converts loss of the replay path into proof that the original delivery did nothing.

This capability applies only to ordinary provider operations. Governed authority-use dispatches keep
their stricter receipt and non-repeat rules. Negotiation changes capability semantics, not the
ControlFrame or Surface API shape.

## Alternatives considered

- **Replay every stable operation:** rejected because stable identity alone does not prove that an
  external provider persists results or suppresses duplicate effects.
- **Keep all takeovers uncertain:** rejected because it prevents deterministic convergence even when
  the adapter explicitly guarantees operation-keyed replay.
- **Infer safety from a transport write error:** rejected because a failed or cancelled WebSocket
  write does not prove that no bytes reached the peer.
- **Add a provider-status request first:** deferred. It would add a new protocol operation and still
  require provider-specific durable evidence; negotiated replay is the smaller contract already
  implemented by the deterministic runtime.

## Consequences

- Existing adapters remain compatible and retain uncertainty because capabilities are optional.
- Adapter authors opt in only when the native provider path can uphold operation-keyed idempotency
  across reconnects and process failure.
- The reference simulator advertises the capability and its ledger proves multiple deliveries can
  produce one effect.
- Provider qualification must test both before-effect abandonment and after-effect result replay;
  negotiation alone is not evidence that a production provider satisfies the promise.
