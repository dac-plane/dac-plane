# dac-plane

`dac-plane` is a provider-neutral control plane for delegated agent work. It owns the durable product
lifecycle—tasks, attempts, attention, decisions, budgets, artifacts, audit, result revisions and
reviews—while agent runtimes and execution environments remain replaceable integrations.

The product is not implemented yet. The reviewed implementation baseline is in
[`docs/architecture/`](docs/architecture/README.md). The earlier candidate research, experiments and
TypeScript/PostgreSQL prototypes are retained under [`architecture-explore/`](architecture-explore/README.md)
as evidence, not as the active product implementation.

Start with:

- [Architecture baseline](docs/architecture/README.md)
- [Pre-implementation architecture review](docs/architecture/review-2026-10-15.md)
- [ADR-0001: Go backend](docs/adr/0001-control-plane-language.md)
- [ADR-0002: modular-monolith control plane](docs/adr/0002-initial-application-boundary.md)

