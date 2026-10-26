# ADR-0011: Roll compatible binaries before schema migrations

- Status: Accepted
- Date: 2026-10-26
- Issues: [#74](https://github.com/dac-plane/dac-plane/issues/74),
  [#87](https://github.com/dac-plane/dac-plane/issues/87)
- Supersedes: the rollout ordering in [ADR-0009](0009-deployment-unit-and-first-substrate.md)

## Context

ADR-0009 requires an explicit migration job and says to migrate before rolling serving replicas. Its
first implementation declares schemas 12 through 13 servable. A later release that introduces schema
14 cannot follow that order safely: the old binary rejects 14 at startup, and a process already
running when the database reaches 14 becomes unready but does not stop its worker. Readiness removes a
replica from new request routing; it is not a fence on background work or an adapter connection.

The claimed expand-then-contract compatibility therefore needs a different order. The new binary is
the component that knows both the old and new schema. It must replace every old binary while the
database is still on the old schema, before any migration makes the old binary incompatible.

## Decision

For a release that changes the schema, use this order:

1. Verify the new image against the database's current schema and confirm that schema is inside the
   new binary's declared serving range.
2. Roll every `serve` replica to the new image while the database remains at the current schema.
   Readiness and normal lifecycle checks must pass after each replacement.
3. Confirm that no old serving process or adapter connection remains.
4. Run the new image's explicit `migrate up` job under the migration advisory lock.
5. Verify the migrated schema, replica readiness and lifecycle progress before completing the
   rollout.

Serving processes still never migrate and still use a DML-only database role. Migrations remain
expand-then-contract: a release that adds a new shape reads the old and new shapes; a later release
may contract only after no supported binary needs the old shape.

Before step 4, rollback means rolling the old image back against the unchanged schema. After step 4,
an old binary may be restored only when its declared serving range includes the migrated schema.
Otherwise the operator rolls forward or performs an explicit, evidence-preserving schema downgrade;
readiness failure is never treated as permission to leave an incompatible worker running.

## Alternatives considered

- **Keep migrate-first and let the old release accept one unknown future version:** rejected. A
  version number alone cannot prove that an unknown future migration is backward compatible, and an
  incompatible worker would retain database and adapter access after readiness failed.
- **Stop every replica, migrate, then start the new image:** rejected. It is safe but introduces
  avoidable control-plane downtime and does not exercise the rolling topology ADR-0009 selected.
- **Have each replica migrate on startup:** rejected for the reasons ADR-0009 records: serving roles
  would need DDL privileges and migration failure would be coupled to crash-loop recovery.
- **Use readiness as the worker fence:** rejected. Readiness is advisory to the substrate and cannot
  revoke database claims or an already established adapter connection.

## Consequences

- Deployment automation needs distinct rollout and migration phases, with an explicit check that no
  old replicas remain between them.
- The next binary must continue serving the current schema until its migration completes.
- Rollback becomes more constrained after the schema changes and must be checked against the old
  binary's declared range.
- Issue #74 proves the sequence with the first deployable image and an isolated next-release schema
  fixture. That evidence validates the deployment mechanism, not an unqualified provider or database
  high availability.
