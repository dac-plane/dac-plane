# Architecture Decision Records

Decisions follow spec §31 ("Decision records"). Status values: **Proposed** (direction set by the spec,
not yet validated), **Accepted** (validated by evidence recorded in the ADR), **Open** (needs a
qualification result or an owner decision before it can be closed).

Nothing in these ADRs claims that a third-party component (OCC/OCE, OpenClaw, OpenShell, E2B,
OpenSandbox, Daytona, Firecracker) has been deployed or tested by this project. All such claims are
"Documented" or "Source-inspected" per spec §45 until a qualification report (docs/qualification) says
otherwise.

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-composition-over-evaluated-foundation.md) | Composition over an evaluated foundation | Proposed |
| [0002](0002-product-task-state-separate-from-resource-state.md) | Product task state separate from OCC resource state | Accepted (for product schema) |
| [0003](0003-explicit-trust-scope.md) | Explicit trust scope / isolation per task | Proposed |
| [0004](0004-pinned-runtime-and-protocol.md) | Pinned runtime and protocol | Open |
| [0005](0005-native-approval-authority-plus-product-projection.md) | Native approval authority plus product projection | Proposed |
| [0006](0006-product-backend-stack.md) | Product backend stack: TypeScript + PostgreSQL | Accepted |
| [0007](0007-product-durability-sql-outbox.md) | Product waits via SQL/outbox; Temporal deferred | Open |
| [0008](0008-sandbox-provider.md) | Sandbox / compute provider selection | Open |
| [0009](0009-adoption-vs-fork.md) | Adoption vs fork | Proposed |
| [0010](0010-one-lifecycle-owner-per-resource.md) | One lifecycle owner per resource | Proposed |
| [0011](0011-first-integration-experiment.md) | First integration experiment + state interpretations | Accepted (provisional) |
| [0012](0012-adoption-decision-occ.md) | Adopt OCC or lighter route | Draft (evidence missing) |

ADR-0006..0008 numbers are not assigned by the spec; they are used here for decisions the spec lists
in §26/§31 without a number.
