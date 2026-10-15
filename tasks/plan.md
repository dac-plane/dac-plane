# Implementation plan: issue #32 provider contract semantics

Tracker: [GitHub issue #32](https://github.com/AssahBismarkabah/dac-plane/issues/32)

## Objective

Promote the transferable Plane-Bot findings into active, provider-neutral architecture requirements
without adopting its vendor topology, transports or implementations. Preserve `dac-plane` as the sole
authoritative lifecycle owner and make the M1 contract-and-simulator target explicit.

## Ordered tasks

1. Add ADR-0003 separating observed evidence, adopted semantics and rejected vendor choices.
2. Update the active architecture baseline with the adopted contract families and simulator obligations.
3. Update the pre-implementation review with the resulting invariants and M1 qualification boundary.
4. Link ADR-0003 from the root README and verify that no implementation or diagram changed.

## Verification

```sh
rg -n "ControlFrame|EventCursor|AuthorityGrant|AuthorityUse|HookRequest|HookResponse|SpendChallenge|ArtifactCommitOutcome|ActionPrecondition" docs/architecture docs/adr
rg -n "ConnectRPC|Temporal|WebAuthn|485|vendor|provider-specific" docs/adr/0003-provider-contract-semantics.md
git diff --check
go test ./...
```

## Boundaries

- No Go contracts, JSON Schemas, database schema or provider implementation is added.
- No production runtime, environment, transport, workflow engine or credential ceremony is selected.
- The architecture diagrams and retained exploration evidence remain unchanged.
