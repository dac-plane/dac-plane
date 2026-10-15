# dac-plane

`dac-plane` is a provider-neutral control plane for delegated agent work. It owns the durable product
lifecycle—tasks, attempts, attention, decisions, budgets, artifacts, audit, result revisions and
reviews—while agent runtimes and execution environments remain replaceable integrations.

The product lifecycle is not implemented yet. The repository contains the initial Go process and
PostgreSQL test foundation; the reviewed implementation baseline is in
[`docs/architecture/`](docs/architecture/README.md). The earlier candidate research, experiments and
TypeScript/PostgreSQL prototypes are retained under [`architecture-explore/`](architecture-explore/README.md)
as evidence, not as the active product implementation.

The first milestone culminates in the
[contract-freeze gate](docs/architecture/review-2026-10-15.md#contract-freeze-gate): authoritative
contracts and migrations for all nine product records, exercised by a deterministic runtime simulator
through one end-to-end lifecycle slice.

Start with:

- [Architecture baseline](docs/architecture/README.md)
- [Pre-implementation architecture review](docs/architecture/review-2026-10-15.md)
- [ADR-0001: Go backend](docs/adr/0001-control-plane-language.md)
- [ADR-0002: modular-monolith control plane](docs/adr/0002-initial-application-boundary.md)
- [ADR-0003: provider-neutral execution contract semantics](docs/adr/0003-provider-contract-semantics.md)

Contribution conventions (commits, PR titles, review rules) are in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Development

The foundation requires Go 1.26.9 or a newer Go 1.26 patch release. Unit tests do not require
external services; integration tests use Docker to start a pinned PostgreSQL 16 container.

```sh
go test ./...
go vet ./...
go test -race ./...
go test -tags=integration ./...
```

The current process requires `DAC_DATABASE_URL` and accepts an optional positive Go duration in
`DAC_DATABASE_CONNECT_TIMEOUT` (default `10s`). It connects to PostgreSQL, waits for SIGINT or SIGTERM,
and shuts down cleanly. No HTTP surface or product lifecycle behavior exists yet.
