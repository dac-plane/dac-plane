-- +goose Up
-- Restricted administration and preview evidence, not provider-transfer outcomes.
CREATE TABLE harness_administrator (
 tenant_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 enabled boolean NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,principal_id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES principal(tenant_id,principal_id)
);
CREATE TABLE migration_administration (
 tenant_id uuid NOT NULL,
 migration_id uuid NOT NULL,
 from_version bigint CHECK (from_version > 0),
 to_version bigint CHECK (to_version > 0),
 preview_at timestamptz,
 PRIMARY KEY (tenant_id,migration_id),
 FOREIGN KEY (tenant_id,migration_id) REFERENCES migration_pass(tenant_id,migration_id),
 CHECK ((preview_at IS NOT NULL) = (from_version IS NOT NULL)),
 CHECK ((preview_at IS NOT NULL) = (to_version IS NOT NULL))
);
-- One reusable tenant-wide administrative claim, never a physical resource.
CREATE TABLE migration_slot (
 tenant_id uuid PRIMARY KEY REFERENCES tenant(id),
 claim_id uuid NOT NULL,
 active_migration_id uuid,
 FOREIGN KEY (tenant_id,claim_id) REFERENCES maintenance_claim(tenant_id,claim_id),
 FOREIGN KEY (tenant_id,active_migration_id) REFERENCES migration_administration(tenant_id,migration_id)
);

-- +goose Down
LOCK TABLE harness_administrator,migration_administration,migration_slot IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM harness_administrator)
 OR EXISTS (SELECT 1 FROM migration_administration)
 OR EXISTS (SELECT 1 FROM migration_slot) THEN
 RAISE EXCEPTION 'cannot downgrade while version 7 migration administration exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE migration_slot;
DROP TABLE migration_administration;
DROP TABLE harness_administrator;
