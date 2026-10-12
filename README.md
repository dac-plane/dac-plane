# Delegated Agent Control Plane

Product task/attention/result layer that lets an individual delegate software work to coding agents,
follow progress, answer questions, approve specific operations and review results with evidence.

Specification: [docs/spec/SPEC.md](docs/spec/SPEC.md) (v1.1, 12 Oct 2026; the
[PDF](docs/spec/delegated-agent-control-plane-v1.1.pdf) is authoritative).

> **Status: phase 0 / milestone 1 foundations.** Contracts, domain model, state machines and the product
> storage schema exist and are tested. No runtime, sandbox, OCC integration, HTTP service or UI exists yet,
> and no reuse qualification experiment (Q1-Q8) has been run.

## Reuse-first
v1.1 builds only product semantics and thin adapters. Agent deployment, IAM, audit, harness and sandbox
lifecycle are reuse candidates (OpenClaw Enterprise/OCC first, lighter runtime + one managed sandbox as
fallback). See [docs/adr](docs/adr/README.md).

## Layout (spec §26)
| Path | Purpose | State |
|------|---------|-------|
| `packages/product-api` | Stable surface contracts: domain types, state machines, approval binding, capability tiers, JSON Schemas, OpenAPI 3.1 | Done (WP02 draft) |
| `services/product-tasks` | Product task service; PostgreSQL schema `product` + constraint tests | Schema done; service not started |
| `integrations/occ` | OCC public API mapping (WP03) | Not started |
| `integrations/runtime` | Native session/event/approval correlation (WP04) | Not started |
| `apps/task-workspace` | First-party web UI (WP08) | Not started |
| `tests/integration-fit` | Reuse qualification and failure tests | Not started |
| `docs/adr` | Decision records | |
| `docs/qualification` | Q1-Q8 reports (all NOT RUN) | |

## Develop
```sh
npm ci
npm run typecheck
npm test                 # vitest: state machines, approval binding, schemas, OpenAPI
# storage (needs PostgreSQL 16+):
psql -v ON_ERROR_STOP=1 -d dacp -f services/product-tasks/migrations/0001_product_core.sql
psql -v ON_ERROR_STOP=1 -d dacp -f services/product-tasks/test/sql/0001_constraints.sql
```

## What is implemented
- State machines for task, attempt, attention request, environment and connection (spec §11, §13, §19),
  with optimistic-version compare-and-set, terminal-state protection and explicit targets when policy
  chooses the outcome (e.g. verification pass -> review vs auto-accept).
- Canonical action hash and approval binding checks (tenant, responder, request version, expiry,
  superseded attempt) plus executor-side revalidation (hash, policy version, lease epoch) (§13, §33).
- Event envelope v1 / command envelope, event-type ingestion rule (unknown permission/state events are
  rejected) and runtime capability tiers A/B/C (§14, §16).
- JSON Schemas and OpenAPI for every §15 endpoint.
- PostgreSQL schema enforcing §10: tenant-scoped composite FKs, one active attempt per task, one blocking
  attention request per attempt, one decision per request version, idempotency keys, unique native event
  keys, per-task event sequence, transactional outbox, append-only audit, immutable artifacts/events,
  lease-fencing claim function.

## Integration spike (ADR-0011)
OCE + dedicated Codex on local k3d: stopped at Q1 on the agent box (no kernel `ip_set`, no model key).
See [spike/README.md](spike/README.md), [capability report](docs/qualification/capability-report.md),
[responsibility map](docs/qualification/responsibility-map.md), draft [ADR-0012](docs/adr/0012-adoption-decision-occ.md).

## Next (backlog, spec §30)
1. WP01 reuse qualification: Q1 OCC deployment, Q2 native attention, Q6 sandbox alternative -> choose Option A or B.
2. WP02 finish: tenant identity mapping to the chosen platform auth; publish contract versioning/deprecation policy.
3. WP09 product outbox publisher + task command handlers in `services/product-tasks` (transition + event + outbox in one transaction).
4. WP04 runtime simulator fixture and adapter for the pinned runtime.
5. WP05 attention inbox API; WP07 artifact manifest/review; WP08 task workspace UI.
