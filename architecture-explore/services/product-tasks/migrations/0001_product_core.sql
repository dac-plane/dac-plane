-- Milestone 1 product schema (spec §9, §10, §11, §13). Product-owned; never shared with OCC tables (ADR-0002).
BEGIN;
CREATE SCHEMA IF NOT EXISTS product;
SET search_path = product;

CREATE TABLE tenant (
  id text PRIMARY KEY,
  policy_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principal (
  id text PRIMARY KEY,
  idp_subject text NOT NULL UNIQUE,  -- from the identity provider; reuse platform auth
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenant_membership (
  tenant_id text NOT NULL REFERENCES tenant(id),
  principal_id text NOT NULL REFERENCES principal(id),
  roles text[] NOT NULL DEFAULT '{}',
  PRIMARY KEY (tenant_id, principal_id)
);

CREATE TABLE connection (
  id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenant(id),
  owner_principal_id text NOT NULL REFERENCES principal(id),
  provider text NOT NULL,
  external_account_id text,
  credential_ref text,              -- opaque reference into managed secret store; never secret material
  scopes text[] NOT NULL DEFAULT '{}',
  state text NOT NULL CHECK (state IN ('requested','authorizing','active','refresh_needed','revoked','expired','error')),
  version integer NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id)
);

CREATE TABLE project (
  id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenant(id),
  repository_connection_id text,
  base_revision text,
  storage_ref text,
  trust_group text NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id),
  FOREIGN KEY (tenant_id, repository_connection_id) REFERENCES connection(tenant_id, id)
);

CREATE TABLE agent_profile_version (
  tenant_id text NOT NULL REFERENCES tenant(id),
  profile_id text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  instructions_ref text NOT NULL,
  runtime_constraints jsonb NOT NULL DEFAULT '{}',
  tool_policy_ref text NOT NULL,
  memory_scope text NOT NULL,
  PRIMARY KEY (tenant_id, profile_id, version)
);

CREATE TABLE task (
  id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenant(id),
  project_id text NOT NULL,
  owner_id text NOT NULL REFERENCES principal(id),
  profile_version text NOT NULL,
  objective text NOT NULL CHECK (length(objective) > 0),
  acceptance_criteria jsonb NOT NULL CHECK (jsonb_typeof(acceptance_criteria) = 'array' AND jsonb_array_length(acceptance_criteria) > 0),
  state text NOT NULL CHECK (state IN ('draft','queued','running','waiting_attention','verifying','ready_for_review','cancelling','succeeded','failed','cancelled')),
  version integer NOT NULL DEFAULT 0,
  sequence bigint NOT NULL DEFAULT 0,   -- per-aggregate event sequence (§10)
  budget_ref text,
  external_effects text NOT NULL DEFAULT 'no_publish' CHECK (external_effects IN ('none','no_publish','draft_only','ask')),
  auto_accept boolean NOT NULL DEFAULT false,
  parent_task_id text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id),
  FOREIGN KEY (tenant_id, project_id) REFERENCES project(tenant_id, id),
  FOREIGN KEY (tenant_id, parent_task_id) REFERENCES task(tenant_id, id)
);

CREATE TABLE environment (
  id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenant(id),
  provider text NOT NULL,
  provider_ref text,                 -- observation/correlation only; provider owns lifecycle (ADR-0010)
  state text NOT NULL CHECK (state IN ('requested','provisioning','ready','active','draining','stopped','destroyed','failed','quarantined')),
  version integer NOT NULL DEFAULT 0,
  trust_group text NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id)
);

CREATE TABLE session_binding (
  id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenant(id),
  native_session_ref text NOT NULL,  -- adapter mapping; never an authorization token
  runtime_version text NOT NULL,
  workspace_id text,
  checkpoint_ref text,
  recovery_mode text NOT NULL CHECK (recovery_mode IN ('exact_resume','restart_from_artifacts','unsupported')),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id)
);

CREATE TABLE attempt (
  id text NOT NULL,
  tenant_id text NOT NULL,
  task_id text NOT NULL,
  runtime_adapter_version text NOT NULL,
  session_binding_id text,
  environment_id text,
  state text NOT NULL CHECK (state IN ('allocated','provisioning','starting','executing','waiting','finalizing','completed','failed','lost','cancellation_requested','terminated')),
  version integer NOT NULL DEFAULT 0,
  lease_epoch bigint NOT NULL DEFAULT 0,
  lease_owner text,
  lease_expires_at timestamptz,
  retry_reason text,
  recovery_mode text CHECK (recovery_mode IN ('exact_resume','restart_from_artifacts','unsupported')),
  started_at timestamptz,
  ended_at timestamptz,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id),
  FOREIGN KEY (tenant_id, session_binding_id) REFERENCES session_binding(tenant_id, id),
  FOREIGN KEY (tenant_id, environment_id) REFERENCES environment(tenant_id, id)
);
-- At most one active attempt per task in v1 (§10). 'lost' still holds authority until reconciled.
CREATE UNIQUE INDEX attempt_one_active_per_task ON attempt (tenant_id, task_id)
  WHERE state NOT IN ('completed','failed','terminated');

CREATE TABLE attention_request (
  id text NOT NULL,
  tenant_id text NOT NULL,
  task_id text NOT NULL,
  attempt_id text NOT NULL,
  type text NOT NULL CHECK (type IN ('question','action_approval','authentication_handoff','blocked_work')),
  native_request_ref text NOT NULL,
  action_hash text CHECK (action_hash ~ '^sha256:[0-9a-f]{64}$'),
  payload_ref text NOT NULL,
  status text NOT NULL CHECK (status IN ('created','pending','answered','dispatching','delivered','consumed','closed','denied','expired','cancelled','superseded')),
  version integer NOT NULL DEFAULT 1,
  blocking boolean NOT NULL DEFAULT true,
  expiry timestamptz NOT NULL,
  policy_version text NOT NULL,
  authorized_responders text[] NOT NULL CHECK (cardinality(authorized_responders) > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id),
  CHECK (type <> 'action_approval' OR action_hash IS NOT NULL)
);
-- One blocking attention request per attempt in v1 (§11).
CREATE UNIQUE INDEX attention_one_blocking_per_attempt ON attention_request (tenant_id, attempt_id)
  WHERE blocking AND status IN ('created','pending','answered','dispatching','delivered');
CREATE UNIQUE INDEX attention_native_ref ON attention_request (tenant_id, attempt_id, native_request_ref);

CREATE TABLE decision (
  id text NOT NULL,
  tenant_id text NOT NULL,
  request_id text NOT NULL,
  request_version integer NOT NULL,
  principal_id text NOT NULL REFERENCES principal(id),
  decision text NOT NULL CHECK (decision IN ('approve_once','deny','answer')),
  answer_ref text,
  decided_at timestamptz NOT NULL DEFAULT now(),
  dispatch_id text,
  delivered_at timestamptz,
  consumed_at timestamptz,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, request_id) REFERENCES attention_request(tenant_id, id),
  -- At most one accepted decision per attention request version (§10)
  UNIQUE (tenant_id, request_id, request_version)
);

CREATE TABLE action_operation (
  id text NOT NULL,
  tenant_id text NOT NULL,
  attempt_id text NOT NULL,
  operation text NOT NULL,
  target jsonb NOT NULL,
  normalized_input_hash text NOT NULL,
  policy_version text NOT NULL,
  grant_ref text,
  lease_epoch bigint NOT NULL,
  execution_status text NOT NULL CHECK (execution_status IN ('recorded','dispatched','succeeded','failed','uncertain','denied')),
  external_operation_ref text,
  external_idempotency_key text,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id)
);

CREATE TABLE grant_ (
  id text NOT NULL,
  tenant_id text NOT NULL,
  connection_id text NOT NULL,
  task_id text,
  profile_id text,
  capabilities text[] NOT NULL,
  expires_at timestamptz,
  revoked_at timestamptz,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, connection_id) REFERENCES connection(tenant_id, id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id),
  CHECK (task_id IS NOT NULL OR profile_id IS NOT NULL)
);

CREATE TABLE artifact (
  id text NOT NULL,
  tenant_id text NOT NULL,
  task_id text NOT NULL,
  attempt_id text NOT NULL,
  uri text NOT NULL,
  digest text NOT NULL,
  classification text NOT NULL CHECK (classification IN ('public','project_internal','confidential','restricted')),
  source_revision text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id)
);

CREATE TABLE verification (
  id text NOT NULL,
  tenant_id text NOT NULL,
  task_id text NOT NULL,
  attempt_id text NOT NULL,
  result_revision text NOT NULL,
  verifier text NOT NULL CHECK (verifier IN ('platform','agent')),
  result text NOT NULL CHECK (result IN ('pass','fail','error')),
  criteria_results jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt(tenant_id, id)
);

-- Event metadata; payloads live in access-controlled objects (§14).
CREATE TABLE event (
  event_id text NOT NULL,
  tenant_id text NOT NULL,
  task_id text NOT NULL,
  attempt_id text,
  sequence bigint NOT NULL,
  type text NOT NULL,
  occurred_at timestamptz NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  producer text NOT NULL,
  correlation_id text NOT NULL,
  causation_id text,
  payload_ref text,
  classification text NOT NULL,
  native_event_key text,
  PRIMARY KEY (tenant_id, event_id),
  UNIQUE (tenant_id, task_id, sequence),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task(tenant_id, id)
);
-- Unique native event key per adapter binding (§10)
CREATE UNIQUE INDEX event_native_key ON event (tenant_id, attempt_id, native_event_key) WHERE native_event_key IS NOT NULL;

-- Unique idempotency key per tenant and API operation (§10)
CREATE TABLE idempotency_key (
  tenant_id text NOT NULL REFERENCES tenant(id),
  operation text NOT NULL,
  key text NOT NULL,
  request_hash text NOT NULL,
  response jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, operation, key)
);

-- Transactional outbox (§10, ADR-0007). Delivered at least once; consumers dedupe by command_id.
CREATE TABLE outbox (
  command_id text PRIMARY KEY,
  tenant_id text NOT NULL REFERENCES tenant(id),
  type text NOT NULL CHECK (type IN ('StartAttempt','SubmitTurn','DeliverDecision','CancelAttempt','VerifyResult','CleanupEnvironment')),
  target_id text NOT NULL,
  expected_version integer,
  lease_epoch bigint,
  deadline timestamptz NOT NULL,
  idempotency_key text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  available_at timestamptz NOT NULL DEFAULT now(),
  attempts integer NOT NULL DEFAULT 0,
  published_at timestamptz,
  UNIQUE (tenant_id, type, idempotency_key)
);
CREATE INDEX outbox_unpublished ON outbox (available_at) WHERE published_at IS NULL;

-- Append-only security audit (§10)
CREATE TABLE audit_log (
  id bigserial PRIMARY KEY,
  tenant_id text NOT NULL,
  actor text NOT NULL,
  action text NOT NULL,
  object_type text NOT NULL,
  object_id text NOT NULL,
  detail jsonb NOT NULL DEFAULT '{}',
  at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on % is not permitted (append-only/immutable)', TG_OP, TG_TABLE_NAME USING ERRCODE = 'P0001';
END $$;
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
-- Artifact digests cannot be silently overwritten (§10)
CREATE TRIGGER artifact_immutable BEFORE UPDATE OR DELETE ON artifact FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER event_immutable BEFORE UPDATE OR DELETE ON event FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Lease fencing (§10): claim with compare-and-set; returns new epoch or no row.
CREATE FUNCTION claim_attempt(p_tenant text, p_attempt text, p_owner text, p_expected_epoch bigint, p_ttl interval)
RETURNS bigint LANGUAGE sql AS $$
  UPDATE product.attempt
     SET lease_epoch = lease_epoch + 1, lease_owner = p_owner, lease_expires_at = now() + p_ttl
   WHERE tenant_id = p_tenant AND id = p_attempt AND lease_epoch = p_expected_epoch
     AND (lease_expires_at IS NULL OR lease_expires_at < now() OR lease_owner = p_owner)
     AND state NOT IN ('completed','failed','terminated')
  RETURNING lease_epoch;
$$;

COMMIT;
