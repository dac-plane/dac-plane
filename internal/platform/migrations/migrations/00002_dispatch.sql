-- +goose Up
-- 00002: fenced dispatch support for issue #21.
--
-- Additive only; 00001 is frozen. The outbox claim columns from 00001
-- (status, claimed_at, claimed_by, lease_epoch, lease_expires_at) are now
-- owned by the dispatch loop; this version adds the per-row retry facts the
-- loop needs: an attempt counter, a not-before timestamp for backoff, the
-- last typed failure, an exhaustion marker that stops polling while keeping
-- the row inspectable, and the demand hint the scheduler orders by.
-- Shadow results get their own append-only table: shadow runs record
-- comparison evidence here and never touch authoritative rows.

ALTER TABLE outbox ADD COLUMN attempts int NOT NULL DEFAULT 0 CHECK (attempts >= 0);
ALTER TABLE outbox ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE outbox ADD COLUMN last_error text;
ALTER TABLE outbox ADD COLUMN exhausted_at timestamptz;
ALTER TABLE outbox ADD COLUMN demand text NOT NULL DEFAULT 'background' CHECK (demand IN ('person', 'background'));

-- Ready poll order for the dispatch loop: pending, unexhausted, due rows by
-- not-before time. Person-ahead-of-background ordering happens in Go over
-- this set; the index serves the due-and-pending filter.
CREATE INDEX outbox_ready ON outbox (next_attempt_at) WHERE status = 'pending' AND exhausted_at IS NULL;

-- Durable shadow-result evidence. One row per shadow run, append-only like
-- audit events: later runs add rows, nothing updates them in place.
CREATE TABLE shadow_result (
  tenant_id          uuid        NOT NULL REFERENCES tenant (id),
  shadow_id          uuid        NOT NULL,
  attempt_id         uuid        NOT NULL,
  captured_at        timestamptz NOT NULL,
  summary            text        NOT NULL CHECK (summary <> ''),
  comparison_summary text        NOT NULL,
  evidence_digest    text        NOT NULL CHECK (evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
  PRIMARY KEY (tenant_id, shadow_id),
  UNIQUE (shadow_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

-- Shadow runs add comparison evidence; they never revise it in place.
CREATE TRIGGER shadow_result_immutable
  BEFORE UPDATE OR DELETE ON shadow_result
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS shadow_result_immutable ON shadow_result;
DROP TABLE IF EXISTS shadow_result;
DROP INDEX IF EXISTS outbox_ready;
ALTER TABLE outbox DROP COLUMN IF EXISTS demand;
ALTER TABLE outbox DROP COLUMN IF EXISTS exhausted_at;
ALTER TABLE outbox DROP COLUMN IF EXISTS last_error;
ALTER TABLE outbox DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE outbox DROP COLUMN IF EXISTS attempts;
