# Architecture Decision Records

Decisions follow spec §31 ("Decision records"). Status values: **Proposed** (direction set by the spec,
not yet validated), **Accepted** (validated by evidence recorded in the ADR), **Open** (needs a
qualification result or an owner decision before it can be closed).

Third-party claims use the evidence labels in spec §45. OCC, Paperclip and CanDoItAll have runtime
evidence; the remaining candidates have source/API evidence at the levels recorded in
`docs/qualification`. Source inspection is not a runtime PASS. The broader vendor categories and the
resulting build boundary are recorded in
[`control-plane-market-landscape.md`](../qualification/control-plane-market-landscape.md).

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-composition-over-evaluated-foundation.md) | Composition over an evaluated foundation | Accepted; foundation search closed |
| [0002](0002-product-task-state-separate-from-resource-state.md) | Product task state separate from candidate resource state | Accepted; OCC mapping closed |
| [0003](0003-explicit-trust-scope.md) | Explicit trust scope / isolation per task | Proposed |
| [0004](0004-pinned-runtime-and-protocol.md) | Pinned runtime and protocol | Open |
| [0005](0005-native-approval-authority-plus-product-projection.md) | Native approval authority plus product projection | Proposed |
| [0006](0006-product-backend-stack.md) | Product backend stack: TypeScript + PostgreSQL | Superseded |
| [0007](0007-product-durability-sql-outbox.md) | Product waits via SQL/outbox; Temporal deferred | Open |
| [0008](0008-sandbox-provider.md) | Sandbox / compute provider selection | Open |
| [0009](0009-adoption-vs-fork.md) | Adoption vs fork | Proposed |
| [0010](0010-one-lifecycle-owner-per-resource.md) | One lifecycle owner per resource | Accepted |
| [0011](0011-first-integration-experiment.md) | First integration experiment + state interpretations | Superseded; negative adoption result |
| [0012](0012-adoption-decision-occ.md) | Do not adopt OCC/OCE as the project runtime dependency now | Rejected for current adoption |
| [0013](0013-control-plane-candidate-qualification.md) | Qualify existing delegated-work control planes before building lifecycle machinery | Accepted; scan closed with no adopted foundation |

ADR-0006..0008 numbers are not assigned by the spec; they are used here for decisions the spec lists
in §26/§31 without a number.
