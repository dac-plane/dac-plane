# ADR-0002: Product task state separate from candidate resource state

- Status: Accepted for the product schema; OCC mapping closed
- Spec: §10, §36, §40

## Decision
Product task/attempt/attention/decision/outbox state lives in a product-owned PostgreSQL schema
(`product`, see `services/product-tasks/migrations`) unless a qualified foundation maps every required
record without dual ownership. The product never writes a candidate's private tables; integrations use
supported public APIs. Environment/runtime readiness means "execution available", never "task
succeeded". A deployment snapshot remains distinct from a result revision.

## Qualification outcome
OCC mapping is closed by ADR-0012. CanDoItAll's bounded direct-run qualification found useful execution,
approval, artifact and usage records. Its separate process/workflow model has not yet been exercised, so
single-owner mapping remains open. The product schema stays authoritative unless that bounded mapping
test proves an adapter can preserve it without dual ownership.
