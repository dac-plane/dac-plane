# ADR-0002: Product task state separate from OCC resource state

- Status: Accepted for the product schema; mapping to OCC remains Open
- Spec: §10, §36, §40

## Decision
Product task/attempt/attention/decision/outbox state lives in a product-owned PostgreSQL schema
(`product`, see `services/product-tasks/migrations`). The product never writes OCC private tables; it
integrates through public APIs only (`integrations/occ`). OCC readiness means "runtime available", never
"task succeeded". AgentRevision (deployment snapshot) is kept distinct from ResultRevision (code result).

## Open
Exact Namespace/Agent/AgentRevision mapping fields depend on the pinned OCC API (WP03, Q1).
