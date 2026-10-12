-- Native correlation for the OCC + dedicated Codex spike (ADR-0011, WP03/WP04).
-- Maps product task/attempt IDs to OCC Agent revision and native gateway session/turn/approval IDs.
-- Product stores references only; OCC and the runtime remain the owners (ADR-0010).
BEGIN;
SET search_path = product;

CREATE TABLE occ_binding (
  tenant_id text NOT NULL,
  attempt_id text NOT NULL,
  occ_installation_id text NOT NULL,
  occ_namespace_id text NOT NULL,
  occ_agent_id text NOT NULL,
  occ_revision_id text NOT NULL,      -- exact AgentRevision (deployment snapshot, not a ResultRevision)
  execution_mode text NOT NULL CHECK (execution_mode IN ('embedded','dedicated')),
  harness text NOT NULL CHECK (harness IN ('openclaw','codex')),
  runtime_image_digest text,
  observed_ready_at timestamptz,
  PRIMARY KEY (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id)
);

CREATE TABLE native_turn (
  tenant_id text NOT NULL,
  attempt_id text NOT NULL,
  native_session_key text NOT NULL,   -- gateway sessionKey (adapter mapping, never authority)
  native_run_id text NOT NULL,        -- gateway run/turn id
  submitted_command_id text NOT NULL, -- our SubmitTurn command id (dedupe)
  state text NOT NULL CHECK (state IN ('submitted','accepted','running','finished','aborted','failed','uncertain')),
  stop_reason text,
  started_at timestamptz NOT NULL DEFAULT now(),
  ended_at timestamptz,
  PRIMARY KEY (tenant_id, attempt_id, native_run_id),
  UNIQUE (tenant_id, submitted_command_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id)
);

-- One native approval id maps to exactly one product attention request (and vice versa).
ALTER TABLE attention_request ADD COLUMN native_run_id text;
CREATE UNIQUE INDEX attention_native_approval ON attention_request (tenant_id, native_request_ref);

COMMIT;
