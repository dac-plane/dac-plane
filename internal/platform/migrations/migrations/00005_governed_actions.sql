-- +goose Up
-- Requested action evidence exists even when authorization refuses dispatch.
ALTER TABLE authority_use DROP CONSTRAINT authority_use_tenant_id_operation_id_fkey;
ALTER TABLE hook_evidence DROP CONSTRAINT hook_evidence_tenant_id_operation_id_fkey;
ALTER TABLE authority_grant ADD COLUMN decision_id uuid;
ALTER TABLE authority_grant ADD FOREIGN KEY (tenant_id, decision_id) REFERENCES decision (tenant_id, decision_id);
ALTER TABLE authority_use ADD COLUMN scope jsonb;
ALTER TABLE authority_use ADD COLUMN decision_id uuid;
ALTER TABLE authority_use ADD FOREIGN KEY (tenant_id, decision_id) REFERENCES decision (tenant_id, decision_id);
ALTER TABLE authority_use DROP CONSTRAINT authority_use_reason_check;
ALTER TABLE authority_use ADD CONSTRAINT authority_use_reason_check CHECK (reason IS NULL OR reason IN ('no_grant', 'cross_tenant', 'wrong_attempt', 'wrong_audience', 'wrong_action', 'wrong_operation', 'input_mismatch', 'scope_mismatch', 'revoked', 'retired', 'not_yet_valid', 'expired', 'stale_generation', 'above_ceiling', 'denied_by_policy', 'hook_rejected'));
ALTER TABLE hook_evidence DROP CONSTRAINT hook_evidence_outcome_check;
ALTER TABLE hook_evidence ADD CONSTRAINT hook_evidence_outcome_check CHECK (outcome IN ('allowed', 'denied', 'missing', 'timed_out', 'malformed', 'unavailable'));
ALTER TABLE decision DROP CONSTRAINT decision_kind_check;
ALTER TABLE decision ADD CONSTRAINT decision_kind_check CHECK (kind IN ('approve_once', 'approve_standing', 'deny', 'answer'));
ALTER TABLE attention_request ADD COLUMN scope jsonb;

CREATE TABLE governed_action (
 tenant_id uuid NOT NULL REFERENCES tenant(id),
 operation_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
 execution jsonb NOT NULL CHECK (jsonb_typeof(execution) = 'object'),
 PRIMARY KEY (tenant_id, operation_id),
 UNIQUE (operation_id),
 FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, attempt_id)
);
CREATE TABLE enforcement_receipt (
 tenant_id uuid NOT NULL REFERENCES tenant(id),
 operation_id uuid NOT NULL,
 receipt jsonb NOT NULL CHECK (jsonb_typeof(receipt) = 'object'),
 PRIMARY KEY (tenant_id, operation_id),
 FOREIGN KEY (tenant_id, operation_id) REFERENCES governed_action(tenant_id, operation_id),
 FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation(tenant_id, operation_id)
);
CREATE TRIGGER governed_action_immutable BEFORE UPDATE OR DELETE ON governed_action FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER enforcement_receipt_immutable BEFORE UPDATE OR DELETE ON enforcement_receipt FOR EACH ROW EXECUTE FUNCTION reject_mutation();

ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));

-- +goose Down
DROP TABLE enforcement_receipt;
DROP TABLE governed_action;
ALTER TABLE attention_request DROP COLUMN scope;
ALTER TABLE authority_use DROP COLUMN decision_id;
ALTER TABLE authority_use DROP COLUMN scope;
ALTER TABLE authority_grant DROP COLUMN decision_id;
-- Downgrade is intentionally refused by these constraints if newer evidence
-- cannot be represented by version 4; history is never silently discarded.
ALTER TABLE decision DROP CONSTRAINT decision_kind_check;
ALTER TABLE decision ADD CONSTRAINT decision_kind_check CHECK (kind IN ('approve_once','deny','answer'));
ALTER TABLE authority_use DROP CONSTRAINT authority_use_reason_check;
ALTER TABLE authority_use ADD CONSTRAINT authority_use_reason_check CHECK (reason IS NULL OR reason IN ('no_grant', 'cross_tenant', 'wrong_attempt', 'wrong_audience', 'wrong_action', 'wrong_operation', 'input_mismatch', 'scope_mismatch', 'revoked', 'retired', 'not_yet_valid', 'expired', 'stale_generation', 'above_ceiling', 'denied_by_policy'));
ALTER TABLE hook_evidence DROP CONSTRAINT hook_evidence_outcome_check;
ALTER TABLE hook_evidence ADD CONSTRAINT hook_evidence_outcome_check CHECK (outcome IN ('allowed','denied','missing','timed_out','malformed'));
ALTER TABLE authority_use ADD FOREIGN KEY (tenant_id,operation_id) REFERENCES provider_operation(tenant_id,operation_id);
ALTER TABLE hook_evidence ADD FOREIGN KEY (tenant_id,operation_id) REFERENCES provider_operation(tenant_id,operation_id);
ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact'));
