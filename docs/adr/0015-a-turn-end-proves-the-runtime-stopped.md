# ADR-0015: A turn end proves the runtime stopped

- Status: Accepted
- Date: 2026-10-30
- Issues: [#101](https://github.com/dac-plane/dac-plane/issues/101)

## Context

A cancelled Attempt terminates only after both the runtime and the environment are observed stopped.
Only `runtime.stopped` or an `interrupted` turn counted as the runtime's evidence, and only while the
Attempt was already awaiting its stops.

Real runtimes end turns in ways that evidence missed. A turn can complete or fail in the moment the
cancellation lands, and it can end before the cancellation arrives, when the Attempt is not yet awaiting
a stop. A runtime has no reason to report an interruption it never received. Those Attempts stayed in
`cancellation_requested` with their reservation held, and no later provider event could release them.

An interrupt is also delivered at most once: the row is acknowledged when the frame is written, and a
provider's failure to act on it reaches nobody. An adapter restart could lose it.

## Decision

The two-party stop stands: an Attempt terminates only after both stops are observed.

A terminal end of every turn bound to the Attempt is runtime stop evidence: `completed`, `failed` or
`interrupted`. A turn `awaiting_input` has not ended the runtime's work and proves nothing. A terminal
turn end while the Attempt awaits its stops applies the runtime stop. A cancellation that finds every
bound turn already terminal applies the runtime stop itself and still sends the interrupt, because the
record may lag a turn the runtime has started since.

A failed turn during cancellation therefore ends the Task as `cancelled`. The cancellation came first,
and the turn's failure stays recorded on its turn end. A result proposed during cancellation is refused
as before and never activates.

The worker repeats the interrupt for an Attempt still awaiting its runtime stop, because an interrupt
changes nothing once the work has ended and may be delivered again. Repeats are paced by the reconcile
interval, capped per pass by the batch limit, and end at the interrupt's own deadline. This does not
change the rule that
reconciliation never dispatches an uncertain effect again: an interrupt is a request to stop, not an
effect.

A runtime adapter reports a stop only once the work has ended, not merely the turn. Codex 0.165.0 ended
an interrupted turn and left its command running under the app-server, so the Codex adapter ends the
thread's background terminals before it reports the interruption. If it cannot, it exits rather than
report a stop: a restart ends the command, and the resumed thread reports the interruption.

A start the runtime rejects ran nothing, so its rejection is runtime stop evidence for an Attempt
awaiting its stops.

## Consequences

- A cancellation no longer depends on a provider reporting a stop it had no reason to report.
- A provider must tolerate a repeated interrupt, including one for a turn that already ended.
- Refused late failures are no longer audit evidence on the cancelled path. The turn end carries the
  failure instead.
- An Attempt with no bound turn still has to report a stop or be resolved through its dispatch: the
  absence of a turn is not evidence that nothing ran.
