# Development guide

## Prerequisites

- Go 1.26.9 or a newer Go 1.26 patch release
- [just](https://just.systems) 1.57 or newer
- Docker, for integration tests and the Compose stack

Every check is a recipe in the root `justfile`, and CI calls the same recipes. Run `just` to list them.

## Running tests

Run the narrowest recipe that covers your change.

| Recipe | What it runs |
|---|---|
| `just test` | Unit tests, with no external services |
| `just test-integration` | Tests against a pinned PostgreSQL 16 container, started with Docker |
| `just test-contracts` | JSON Schema conformance and the contract coverage guards |
| `just test-conformance` | The adapter protocol conformance suite |
| `just test-codex-adapter`, `just test-e2b-adapter` | One provider adapter's tests |
| `just test-stack` | A clean TLS Compose stack, driven through the full public API lifecycle |
| `just ci-fast` | The broad local gate, with no deployment or integration suites |
| `just ci` | Everything CI runs, one after another |

Before opening a pull request, run the focused checks for your change and `git diff --check`. Pull requests run the full gate in parallel.

## Running the stack

`just stack-up` starts the reference deployment with simulated providers, and `just stack-down` stops it. The [development profile](../deploy/compose/README.md#development-profile) explains what it generates. Its issuer, identities and results are test fixtures, not evidence that a real provider behaves correctly.

## Repository layout

| Path | What it holds |
|---|---|
| `cmd/dac-plane` | The control-plane process |
| `internal/` | Application modules |
| `adapterkit/` | The adapter kit, a separate Go module |
| `adapters/` | The Codex, E2B and S3 adapters, each a separate Go module |
| `contracts/` | The JSON Schema and OpenAPI contracts, tested against the Go types |
| `deploy/compose/` | The reference deployment |
| `docs/` | Architecture, decision records and release notes |
| `architecture-explore/` | Retained research. Evidence, not the active product. |
