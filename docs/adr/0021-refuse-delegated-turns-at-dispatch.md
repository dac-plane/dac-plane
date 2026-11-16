# ADR-0021: Refuse a delegated turn at dispatch, per adapter connection

- Status: Accepted
- Date: 2026-11-16
- Issue: [#136](https://github.com/dac-plane/dac-plane/issues/136)
- Supersedes: the submission-time refusal in [ADR-0020](0020-agents-are-product-records.md)

## Context

ADR-0020 sends a delegated task's role and instructions in `start_turn`, only to an adapter that negotiated the
`agent.instructions` capability, and refuses a submission whose runtime adapter has not negotiated it.

Capabilities are selected per connection, in each session's hello and ready
([ADR-0008](0008-adapter-process-boundary-and-wire-transport.md),
[ADR-0012](0012-negotiate-stable-operation-replay.md)). A registration can have no connection, or connections built
with different kits during a rolling upgrade, and the set changes between a submission and the dispatch that follows
it. Submission therefore cannot know which connection will carry the turn: a check there would refuse work an
upgraded adapter could run minutes later, and admit work no connection can carry by the time it is dispatched.
Comparable session protocols put the rule where the session is: the Model Context Protocol requires both parties to
use only capabilities that were successfully negotiated in that session.

## Decision

**Submission does not check the runtime's capabilities.** A task may be delegated to an active agent whatever adapters
are connected, as a task may be submitted to a registered runtime with none connected.

**The worker decides per send.** A delegated `start_turn` goes only over a connection of its runtime that selected
`agent.instructions`, and replays only over one that also selected `operation.replay`. When no live connection
selected it, the turn is refused before anything is sent, with the dispatch reason `agent_instructions_unsupported`,
and the task fails visibly. A draining connection that selected it counts as live, so a token renewal releases the send
rather than failing the task. A delegated migration restart with no such connection is held, as one with no
connection at all is.

**The field is added per send.** The recorded dispatch arguments stay as submitted. The worker adds the pinned
revision's role and instructions when it sends, so every delivery of one operation carries the same bytes, and an
adapter that did not select the capability never receives the field.

## Alternatives considered

- **Refuse at submission, as ADR-0020 states:** rejected for the reasons above. It also couples the Surface API's
  answer to the adapter endpoint's live state on another replica.
- **Hold a delegated turn until a capable connection appears:** rejected. A runtime that never upgrades would keep
  the task running and its reservation held without end; a failure names the cause and the owner can resubmit.
- **Send the field and let an old adapter refuse it:** rejected in ADR-0020 already; the strict codec would drop the
  connection.

## Consequences

- A surface learns of an incapable runtime from the task failing with `agent_instructions_unsupported`, not from the
  submission's response. The user guide says so.
- The registry exposes a port scoped to the connections that selected a capability, which later capabilities reuse.
