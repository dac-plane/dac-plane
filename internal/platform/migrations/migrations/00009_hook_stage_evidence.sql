-- +goose Up
-- Keep historical pre-action evidence intact. Follow-up hook stages have a
-- separate natural key so each stage is independently replayable.
ALTER TABLE hook_evidence
  ADD COLUMN duration_ns bigint CHECK (duration_ns IS NULL OR (duration_ns >= 0 AND duration_ns <= 86400000000000)),
  ADD COLUMN response_digest text CHECK (response_digest IS NULL OR response_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN user_message_digest text CHECK (user_message_digest IS NULL OR user_message_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN agent_message_digest text CHECK (agent_message_digest IS NULL OR agent_message_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN context_digest text CHECK (context_digest IS NULL OR context_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN usage jsonb CHECK (usage IS NULL OR (jsonb_typeof(usage)='array' AND jsonb_array_length(usage)<=16));

CREATE TABLE hook_stage_evidence (
  tenant_id uuid NOT NULL REFERENCES tenant(id),
  operation_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind IN ('post_action','action_failure')),
  input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
  output_digest text CHECK (output_digest IS NULL OR output_digest ~ '^sha256:[0-9a-f]{64}$'),
  failure_code text CHECK (failure_code IS NULL OR failure_code IN ('protocol_mismatch','unsupported_capability','malformed_frame','unknown_operation','invalid_arguments','unauthenticated','unauthorized','cancelled','deadline_exceeded','transport_unavailable','provider_failed','stale_precondition','resource_busy')),
  duration_ns bigint NOT NULL CHECK (duration_ns >= 0 AND duration_ns <= 86400000000000),
  response_digest text NOT NULL CHECK (response_digest ~ '^sha256:[0-9a-f]{64}$'),
  user_message_digest text CHECK (user_message_digest IS NULL OR user_message_digest ~ '^sha256:[0-9a-f]{64}$'),
  agent_message_digest text CHECK (agent_message_digest IS NULL OR agent_message_digest ~ '^sha256:[0-9a-f]{64}$'),
  context_digest text CHECK (context_digest IS NULL OR context_digest ~ '^sha256:[0-9a-f]{64}$'),
  usage jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(usage)='array' AND jsonb_array_length(usage)<=16),
  PRIMARY KEY (tenant_id, operation_id, kind),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation(tenant_id, operation_id),
  CHECK ((kind='post_action') = (output_digest IS NOT NULL)),
  CHECK ((kind='action_failure') = (failure_code IS NOT NULL))
);

CREATE TRIGGER hook_stage_evidence_immutable
  BEFORE UPDATE OR DELETE ON hook_stage_evidence
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
LOCK TABLE hook_stage_evidence IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM hook_stage_evidence) OR EXISTS (SELECT 1 FROM hook_evidence WHERE response_digest IS NOT NULL OR duration_ns IS NOT NULL OR user_message_digest IS NOT NULL OR agent_message_digest IS NOT NULL OR context_digest IS NOT NULL OR usage IS NOT NULL) THEN
  RAISE EXCEPTION 'cannot downgrade while enriched hook evidence exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE hook_stage_evidence;
ALTER TABLE hook_evidence DROP COLUMN usage,DROP COLUMN context_digest,DROP COLUMN agent_message_digest,DROP COLUMN user_message_digest,DROP COLUMN response_digest,DROP COLUMN duration_ns;
