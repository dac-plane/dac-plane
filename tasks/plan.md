# Implementation plan: issue #12 Go foundation

Tracker: [GitHub issue #12](https://github.com/AssahBismarkabah/dac-plane/issues/12)

## Objective

Bootstrap the smallest runnable Go modular monolith: validated configuration, a PostgreSQL connection,
graceful process lifetime, hermetic PostgreSQL 16 integration tests, and CI. Domain records, migrations,
HTTP, workers, and provider behavior remain out of scope.

## Ordered tasks

1. Initialize the Go module and configuration contract.
2. Add PostgreSQL and Goose provider primitives with Testcontainers verification.
3. Add the bootstrap composition root and cancellable command process.
4. Add CI and concise development documentation.

## Checkpoints

- After tasks 1-2: unit, vet, race, and tagged PostgreSQL integration tests pass.
- After tasks 3-4: the complete verification command set passes locally and in CI.

## Decisions

- Go packages remain internal; no public Go or HTTP API is introduced.
- Integration tests use a pinned PostgreSQL 16 Testcontainers image.
- Goose is consumed as a PostgreSQL provider library; the generic multi-database CLI is not added.
