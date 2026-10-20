-- +goose Up
-- A migration pass snapshots source Attempts and records the fenced outcome
-- of each transfer. The target identity is intentionally a fresh Attempt:
-- this product does not claim native-session resume across harnesses.
CREATE TABLE migration_item (
 tenant_id uuid NOT NULL,
 migration_id uuid NOT NULL,
 source_attempt_id uuid NOT NULL,
 target_attempt_id uuid,
 state text NOT NULL CHECK (state IN ('queued','quiescing','starting','cutover','cleaning_up','completed','failed','unresolved')),
 claim_epoch bigint NOT NULL CHECK (claim_epoch > 0),
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,migration_id,source_attempt_id),
 UNIQUE (tenant_id,migration_id,target_attempt_id),
 FOREIGN KEY (tenant_id,migration_id) REFERENCES migration_pass(tenant_id,migration_id),
 FOREIGN KEY (tenant_id,source_attempt_id) REFERENCES attempt(tenant_id,attempt_id),
 FOREIGN KEY (tenant_id,target_attempt_id) REFERENCES attempt(tenant_id,attempt_id),
 CHECK (target_attempt_id IS NULL OR target_attempt_id <> source_attempt_id),
 CHECK ((state IN ('cutover','cleaning_up','completed')) = (target_attempt_id IS NOT NULL))
);

-- +goose Down
LOCK TABLE migration_item IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM migration_item) THEN
  RAISE EXCEPTION 'cannot downgrade while version 8 harness migration execution exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE migration_item;
