# ADR-0002: Start with a modular-monolith control plane

- Status: Accepted
- Date: 2026-10-15

## Context

The product needs an API, lifecycle state machine, transactional outbox, dispatch and reconciliation
loops, and replaceable runtime, environment and enforcement adapters. The first container diagram split
these responsibilities into a control-plane service, dispatcher and adapter worker before any measured
security, scaling or availability requirement justified three independently running applications.

That split would create remote failure modes and deployment work before proving the lifecycle model. It
would also obscure the central rule that one product application owns lifecycle transitions and is the
only writer of authoritative product records.

## Decision

Build one control-plane application with explicit internal modules for:

- surface API, authentication and actor authorization;
- task, attempt, attention, decision, budget, artifact and result/review lifecycle;
- transactional command/outbox persistence;
- dispatch, lease fencing and reconciliation loops; and
- runtime, environment and action-enforcement ports plus normalized event ingestion.

PostgreSQL and artifact storage are separate data stores. Agent runtimes, environment providers and
action-enforcement mechanisms are external systems behind ports. They do not write the product database.

The first runtime integration is a deterministic simulator that implements the same port and event
contract as a real adapter. The application may run API and background loops in one process initially.
Operational roles may be split into separate processes later without splitting lifecycle ownership.

## Split triggers

Create a separate worker container only when evidence requires a runtime boundary, such as:

- materially different network or credential access;
- isolation from adapter or provider failure;
- independent scaling or availability targets; or
- measured contention that cannot be addressed inside the application.

Any split requires an ADR defining the protocol, authentication, idempotency, cancellation and failure
reconciliation behavior across the new boundary.

## Consequences

- The first implementation has fewer distributed failure modes and a smaller deployment surface.
- Internal package boundaries must prevent HTTP handlers and adapters from bypassing lifecycle commands.
- Background work remains durable: process-local queues are not authoritative, and committed work is
  recoverable from PostgreSQL.
- A future process split is an operational change, not permission to introduce a second lifecycle owner.

