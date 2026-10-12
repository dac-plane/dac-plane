# services/product-tasks

Product task service: outcomes, attention, budgets, acceptance (spec §26). Milestone 1 contains the
product-owned PostgreSQL schema only; the HTTP service is the next item (see root README).

- `migrations/0001_product_core.sql` — schema `product`: tenant, principal, membership, connection,
  project, agent_profile_version, task, environment, session_binding, attempt, attention_request,
  decision, action_operation, grant_, artifact, verification, event, idempotency_key, outbox, audit_log;
  `claim_attempt()` lease fencing function.
- `test/sql/0001_constraints.sql` — constraint tests for the §10 invariants (runs in CI against Postgres 16).

Run locally: `psql -v ON_ERROR_STOP=1 -d <db> -f migrations/0001_product_core.sql && psql -v ON_ERROR_STOP=1 -d <db> -f test/sql/0001_constraints.sql`
