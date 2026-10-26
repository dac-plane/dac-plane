# dac-plane

`dac-plane` is a provider-neutral control plane for delegated agent work. It owns the durable product
lifecycle—tasks, attempts, attention, decisions, budgets, artifacts, audit, result revisions and
reviews—while agent runtimes and execution environments remain replaceable integrations.

The repository implements the lifecycle through an authenticated Surface API, the adapter-initiated
ControlFrame transport, PostgreSQL persistence, fenced workers and deterministic provider simulators.
The control plane is packaged as a non-root distroless image with an explicit migration step and a
reference Compose stack. No production provider is qualified yet. The reviewed implementation baseline is in
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
just        # list every check
just ci     # run what CI runs
```

Checks are recipes in the root `justfile`, which CI also calls; it needs
[just](https://just.systems) 1.57 or newer.

`dac-plane serve` exposes the Surface API and adapter endpoint over TLS and a local operations
listener for container health checks. `dac-plane migrate` owns schema rollout, `dac-plane provision`
owns trusted bootstrap changes, and secret-bearing settings accept mounted files. See the
[reference deployment guide](deploy/compose/README.md) for configuration, database privileges,
upgrade/rollback rules and the simulator-backed development profile.

```sh
just test-stack  # clean TLS Compose stack and full public-API lifecycle smoke
```
