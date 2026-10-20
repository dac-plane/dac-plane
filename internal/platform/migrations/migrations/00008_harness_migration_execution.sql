-- +goose Up
-- A migration pass snapshots source Attempts and records the fenced outcome
-- of each transfer. The target identity is intentionally a fresh Attempt:
-- this product does not claim native-session resume across harnesses.
CREATE TABLE migration_item (
 tenant_id uuid NOT NULL,
 migration_id uuid NOT NULL,
 source_attempt_id uuid NOT NULL,
 target_attempt_id uuid,

 quiesce_operation_id uuid,
 environment_operation_id uuid,
 restart_operation_id uuid,
 cleanup_operation_id uuid,
 target_cleanup_operation_id uuid,
 context_refs jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(context_refs)='array' AND jsonb_array_length(context_refs)<=32),
 environment_ref text CHECK (environment_ref IS NULL OR length(environment_ref) BETWEEN 1 AND 256),
 reason text CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 128),
 state text NOT NULL CHECK (state IN ('queued','quiescing','starting','cutover','cleaning_up','aborting','completed','failed','unresolved')),
 claim_epoch bigint NOT NULL CHECK (claim_epoch > 0),
 quiesce_epoch bigint NOT NULL DEFAULT 0 CHECK (quiesce_epoch >= 0),
 environment_epoch bigint NOT NULL DEFAULT 0 CHECK (environment_epoch >= 0),
 restart_epoch bigint NOT NULL DEFAULT 0 CHECK (restart_epoch >= 0),
 cleanup_epoch bigint NOT NULL DEFAULT 0 CHECK (cleanup_epoch >= 0),
 target_cleanup_epoch bigint NOT NULL DEFAULT 0 CHECK (target_cleanup_epoch >= 0),
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,migration_id,source_attempt_id),
 UNIQUE (tenant_id,migration_id,target_attempt_id),
 FOREIGN KEY (tenant_id,migration_id) REFERENCES migration_pass(tenant_id,migration_id),
 FOREIGN KEY (tenant_id,source_attempt_id) REFERENCES attempt(tenant_id,attempt_id),
 FOREIGN KEY (tenant_id,target_attempt_id) REFERENCES attempt(tenant_id,attempt_id),
 CHECK (target_attempt_id IS NULL OR target_attempt_id <> source_attempt_id),
 CHECK ((target_attempt_id IS NULL) = (target_cleanup_operation_id IS NULL)),
 CHECK ((state = 'unresolved') = (reason IS NOT NULL)),
 CHECK (quiesce_operation_id IS NOT NULL OR state IN ('queued','unresolved')),
 CHECK (state NOT IN ('starting','cutover','cleaning_up','completed') OR target_attempt_id IS NOT NULL),
 CHECK (state NOT IN ('starting','cutover','cleaning_up','completed') OR restart_operation_id IS NOT NULL),
 CHECK (state NOT IN ('starting','cutover','cleaning_up','completed') OR environment_operation_id IS NOT NULL),
 CHECK (state NOT IN ('cleaning_up','completed') OR cleanup_operation_id IS NOT NULL),
 CHECK (state <> 'aborting' OR cleanup_operation_id IS NOT NULL),
 CHECK (quiesce_epoch <= claim_epoch AND environment_epoch <= claim_epoch AND restart_epoch <= claim_epoch AND cleanup_epoch <= claim_epoch AND target_cleanup_epoch <= claim_epoch)
);

ALTER TABLE attempt DROP CONSTRAINT attempt_state_check;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_check CHECK (state IN ('allocated', 'provisioning', 'starting', 'executing', 'waiting_input', 'finalizing', 'cancellation_requested', 'reconciling', 'migrating', 'migrated', 'completed', 'failed', 'terminated'));

-- A pass has one immutable tenant-wide source snapshot. Claim takeover only
-- advances item fences; it never enrolls Attempts created after execution.
CREATE TABLE migration_snapshot (
 tenant_id uuid NOT NULL,
 migration_id uuid NOT NULL,
 claim_epoch bigint NOT NULL CHECK (claim_epoch > 0),
 snapshotted_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,migration_id),
 FOREIGN KEY (tenant_id,migration_id) REFERENCES migration_pass(tenant_id,migration_id)
);

-- +goose Down
LOCK TABLE migration_item IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM migration_item) OR EXISTS (SELECT 1 FROM migration_snapshot) THEN
  RAISE EXCEPTION 'cannot downgrade while version 8 harness migration execution exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE migration_item;
DROP TABLE migration_snapshot;
ALTER TABLE attempt DROP CONSTRAINT attempt_state_check;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_check CHECK (state IN ('allocated', 'provisioning', 'starting', 'executing', 'waiting_input', 'finalizing', 'cancellation_requested', 'reconciling', 'completed', 'failed', 'terminated'));
