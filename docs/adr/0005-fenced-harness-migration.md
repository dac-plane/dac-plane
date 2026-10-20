# ADR-0005: Fenced harness migration restarts work with explicit evidence

- Status: Accepted
- Date: 2026-10-20

## Context

ADR-0003 adopts provider-neutral migration preview and execution intent, while
ADR-0004 adds restricted administration. Neither makes an administrative
execute record evidence that a provider transfer occurred. Runtime sessions,
environment resources and operations remain provider-owned resources bound to
product-owned Attempts.

## Decision

Execute a bounded migration pass through the existing worker and replaceable
runtime/environment ports. Freeze the source Attempt set once per pass. For each
eligible source, require an exact receipt for source quiescence, target
environment preparation, target runtime start and source environment cleanup.
Each stage has a stable provider operation identity and a durable record of the
claim epoch used in its request. Claim takeover advances the current fence but
does not mutate an earlier provider request's identity or payload.

Create a new Attempt for the same Task on the target harness. Transfer only the
unused reservation amount inside the existing Task budget; do not increase its
ceiling. Preserve the source Attempt and bindings as history and mark that
Attempt `migrated` only after the target has started. Complete the pass only
after source cleanup is confirmed. Missing adapters and uncertain receipts stay
pending; they do not imply success.

The supported continuity is a provider-reported context/checkpoint followed by
restart-from-artifacts/context in a distinct target session. It is not exact
native-session resume or proof of semantic equivalence. Stable-operation replay
may recover the same receipt where the provider guarantees idempotency for that
operation identity; changed stage payloads are not replayed as the same work.

Cancellation is supported before execute. After execution begins, cancellation
is refused because the current adapter contract cannot safely reverse an
unknown or partially applied external effect. The pass and tenant slot remain
visible for recovery rather than being marked cancelled while resources may
still be active.

## Alternatives considered

Treating execute as completion would fabricate transfer evidence. Reusing the
source session or Attempt would conflate provider identity with product
lifecycle. Resetting a stage epoch on takeover would change the payload under a
stable operation identity. Automatically cancelling an executing pass would
hide partial external effects. A new workflow service or provider-specific
resume API is outside the bounded simulator campaign.

## Consequences

Migration is auditable and recoverable through stable operations, current claim
fencing, immutable stage epochs, Attempt history and existing budget records.
Uncertain work can remain pending indefinitely if its provider cannot return
the exact operation receipt. In-flight cancellation, production-provider
qualification and exact native-session continuity are not claimed.
