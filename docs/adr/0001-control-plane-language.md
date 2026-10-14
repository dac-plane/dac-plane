# ADR-0001: Use Go for the control-plane backend

- Status: Accepted
- Date: 2026-10-14
- Supersedes: [`architecture-explore/docs/adr/0006-product-backend-stack.md`](../../architecture-explore/docs/adr/0006-product-backend-stack.md)

## Context

The product needs a durable control-plane service, dispatcher/reconciler workers, runtime and
environment adapters, cancellation propagation, leases, retries, audit records and operationally
simple deployment. The backend language choice was narrowed to Go or Rust.

## Decision

Use Go for the control-plane backend and workers. Keep public contracts language-neutral through
OpenAPI 3.1 and JSON Schema 2020-12. Use PostgreSQL as the authoritative product-state store.

Go fits this workload better because its standard library, explicit cancellation model, lightweight
concurrency and operational tooling align directly with API services, reconciliation loops and adapter
processes. It also keeps build and deployment mechanics small while the product model is still being
proved.

## Why not Rust

Rust provides stronger compile-time memory and data-race guarantees and remains appropriate for a
future isolation-sensitive daemon or performance-critical component. Those benefits do not currently
offset the additional implementation complexity and slower iteration for a database-backed
orchestration service whose dominant risks are lifecycle correctness, idempotency and integration
behavior rather than unsafe memory access.

## Consequences

- The first service and worker modules use Go and ordinary Go package boundaries.
- API and event schemas remain independent of Go types so adapters can be implemented in other
  languages when necessary.
- Concurrency must still be bounded; goroutines are not a substitute for durable queues, leases or
  idempotent state transitions.
- The architecture diagrams remain language-neutral. This ADR, not a box label, owns the language
  decision.
