# Architecture exploration

This directory contains the architecture exploration, specifications, decision records, qualification
evidence and initial contract prototypes produced before implementation of `dac-plane` begins.

The exploration is complete. No evaluated candidate qualified as the provider-neutral lifecycle
foundation. The resulting direction is to build the minimum product-owned lifecycle core for tasks,
attempts, attention, decisions, budgets, artifacts, audit and result review, while keeping agent runtimes,
execution environments, policy systems and infrastructure replaceable.

## Start here

| Document | Purpose |
|----------|---------|
| [Build specification](docs/spec/SPEC.md) | Product boundary, domain model, contracts, state machines, security requirements and phased build plan. |
| [Market landscape](docs/qualification/control-plane-market-landscape.md) | What existing companies and projects control, what remains reusable, and why no horizontal lifecycle foundation was adopted. |
| [Candidate qualification](docs/qualification/control-plane-candidates.md) | Qualification gates, candidate dispositions and the evidence required to reopen the foundation decision. |
| [Architecture decisions](docs/adr/README.md) | Accepted, proposed, rejected and open architecture decisions. |
| [Qualification reports](docs/qualification/README.md) | Runtime tests, source screens and capability evidence. |
| [Implementation status](docs/implementation-status.md) | Existing prototypes, verification commands and the next implementation slice. |
| [Plane-Bot exploration](plane-bot-explore/README.md) | Static Grok Bot architecture findings and compact derived digests used to refine provider-neutral contracts. |

Historical OCC/OpenClaw experiments are retained under [`spike/`](spike/README.md). They are evidence
for rejected approaches, not the active implementation path.

The Plane-Bot exploration publishes its written findings and compact, non-executable digests. Raw REA
output, extracted application copies, native binary slices and analysis tooling remain local and are
intentionally excluded from version control.
