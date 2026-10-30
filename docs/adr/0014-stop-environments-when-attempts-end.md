# ADR-0014: Stop an Attempt's environment when the Attempt ends

- Status: Accepted
- Date: 2026-10-30
- Issues: [#102](https://github.com/dac-plane/dac-plane/issues/102)

## Context

An assigned environment is provisioned for one Attempt. Cancellation stopped it, but a completed or
failed Attempt released its budget and left the resource running until the provider ended it, with no
product record that anything was still live.

The historical specification orders its workflow as review, then "stop compute; retain outputs under
retention policy", and says workspace retention is separate from compute termination. Exploration
ADR-0010 makes the environment provider the owner of compute lifecycle: the product may request a
transition but does not reconcile the resource as a second controller.

In the implemented lifecycle an Attempt completes only after its result revision is verified, so its
outputs are already admitted artifacts. Rework creates a new Attempt with its own environment
operation, so no later Attempt reuses a finished one's resource, and human review has no time bound.

## Decision

When an Attempt completes or fails with a bound environment that is not observed stopped, the same
transaction records and queues one environment stop through the provider contract. The stop is
followed until it is observed, and that observation is accepted on the ended Attempt.

The stop runs at Attempt completion rather than after review. Nothing reads the environment after
completion, and holding compute through an unbounded review would spend it on no work.

Migration keeps its own cleanup operations. A cancelled Attempt keeps its existing stop, which remains
required evidence for termination. A stop on an ended Attempt is cleanup rather than evidence: it never
changes the Attempt, the Task, the result or the released reservation.

## Consequences

- Provider work no longer outlives the Attempt that paid for it on any non-migration path that ends an
  Attempt.
- What a stop does to the workspace stays the provider's choice. The E2B adapter deletes the sandbox,
  so a workspace does not survive its Attempt; outputs must be admitted artifacts.
- A retained or resumable workspace would be a new decision with an explicit retention policy. It is
  not implied by this one.
- A stop can still fail, and an ended Attempt has nothing waiting on it. Detecting those resources needs
  a provider-side lifetime or an orphan sweep, which is separate work.
