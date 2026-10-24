# ADR-0010: Open provider event streams with subscribe and unsubscribe frames

- Status: Accepted
- Date: 2026-10-24
- Issue: [#72](https://github.com/dac-plane/dac-plane/issues/72)

## Context

[ADR-0003](0003-provider-contract-semantics.md) defines the ControlFrame families (lifecycle,
request, cancel, reply, event) and a replayable observation stream bound to a cursor: tenant,
Attempt, provider operation, stream, generation and the highest updated sequence ingested. The
product side opens a stream with `contracts.Port.Subscribe(cursor)` and reads frames until the
provider closes it, resubscribing from the durable cursor after a reconnect or a rehydration.

M1 exercised streams only through in-process ports, so `Subscribe` never needed a frame. Over the wire
transport [ADR-0008](0008-adapter-process-boundary-and-wire-transport.md) decides, the adapter sends
"event frames for the streams the control plane subscribed to", but no frame expresses the
subscription: none of the five families carries a cursor from the control plane to the adapter, and
none closes a stream. The [M1 contract freeze](../architecture/contract-freeze-30.md) records the
frame set as qualified for in-process adapters only.

The Plane-Bot evidence ([deep dive 04](../../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-04.md)
§1) shows a watch stream opened by an explicit request that names where to resume, answered with
`connected`, then rows, cleared, cursor-too-old and heartbeat frames. ADR-0003 adopted those stream
frame kinds; only the opening message is missing.

## Decision

Add two frame kinds to ControlFrame protocol version 1.

- **`subscribe`** carries a complete cursor and is sent by the control plane. It opens that stream on
  the session that carries it. The adapter answers with `event` frames whose binding names the
  cursor's tenant, Attempt, operation and stream, starting with `connected`, and resumes after the
  cursor's updated sequence or answers `cursor_too_old` or `cleared` as ADR-0003 defines.
- **`unsubscribe`** names an operation and stream, and is sent by the control plane. It closes the
  stream that a subscribe opened on the same session. The adapter stops sending events for it.

A session holds at most one open stream per (operation, stream). A second subscribe for the same pair
replaces the first, which is how the control plane reopens a stream from zero after rehydration.

Event frames are routed to the open stream on the same session by (operation, stream). Before a frame
reaches the reconciler, the control plane checks the event's claimed binding against the session's
authenticated peer and the subscribed cursor, with the rules of `BindEvent`. A frame that fails that
check, or arrives for no open stream, is dropped and counted, never applied: a late frame after
unsubscribe is normal, and a frame for another provider's stream is refused by binding, not by
closing a session other streams depend on. A session that ends closes all its streams with a
transport error, and the reconciler resumes from the durable cursor on its next pass.

The adapter sends only lifecycle, reply and event frames. The control plane sends only lifecycle,
request, cancel, subscribe and unsubscribe frames. A frame of the other direction ends the session
with a `malformed_frame` shutdown.

The protocol version stays 1. No adapter outside this repository speaks ControlFrame yet, and the
adapter kit and the control plane ship the extended frame set together; the first published kit
release is the first external contract.

## Alternatives considered

- **A transport-level request method, such as `stream.subscribe`:** rejected. Methods are a per-role
  catalogue of provider operations with durable delivery and reply semantics; a subscription has
  neither, and a reply would add correlation a stream does not need.
- **The adapter pushes every stream unsolicited:** rejected. The control plane decides which
  operations it reconciles and from which cursor; unsolicited streams would send history the product
  never asked for and leave resume points to the adapter.
- **Implicit subscription on reconnect:** rejected. Cursors live in the product database; the adapter
  cannot know them without being told.
- **Bump the protocol to version 2:** rejected for now. Version negotiation exists for released peers;
  there are none yet, and a version that only adds what version 1 always needed would be noise.

## Consequences

- `internal/contracts` gains `Subscribe` and `Unsubscribe` bodies and frame kinds, and
  `contracts/jsonschema/adapter/frame.schema.json` gains them, with agreement tests.
- The adapter kit mirrors both frames, and its conformance suite checks subscription, resume after
  reconnect, cursor-too-old and unsubscribe.
- After the first kit release, changes to the frame set follow version negotiation.
