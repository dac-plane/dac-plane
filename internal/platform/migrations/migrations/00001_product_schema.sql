-- +goose Up
-- 00001: authoritative product schema for the contracts frozen by gate #18.
--
-- Every row carries tenant_id with a foreign key to tenant; record tables use
-- PRIMARY KEY (tenant_id, id) plus a global UNIQUE (id). State and enum CHECK
-- lists mirror the frozen Go constants exactly; Go Validate remains the
-- authoritative check, these constraints are the durable backstop (#20 commits
-- a returned record together with its audit event and outbox entry).
--
-- Protocol-only values are not rows: control frames, hook requests/responses
-- and action preconditions cross the adapter boundary at dispatch time (#21);
-- only hook_evidence digests and stream_cursor progress are stored. Attempt
-- lease columns are nullable support for fenced dispatch (#21 owns the
-- semantics). OCC prototype tables are deliberately not reproduced.

CREATE TABLE tenant (
  id         uuid        PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principal (
  tenant_id    uuid NOT NULL REFERENCES tenant (id),
  principal_id uuid NOT NULL,
  kind         text NOT NULL CHECK (kind IN ('human', 'service')),
  PRIMARY KEY (tenant_id, principal_id),
  UNIQUE (principal_id)
);

CREATE TABLE policy (
  tenant_id  uuid   PRIMARY KEY REFERENCES tenant (id),
  generation bigint NOT NULL CHECK (generation > 0),
  settings   jsonb  CHECK (settings IS NULL OR jsonb_typeof(settings) = 'object'),
  ceiling    jsonb  CHECK (ceiling IS NULL OR jsonb_typeof(ceiling) = 'object'),
  denies     jsonb  CHECK (denies IS NULL OR jsonb_typeof(denies) = 'array'),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task (
  tenant_id  uuid        NOT NULL REFERENCES tenant (id),
  task_id    uuid        NOT NULL,
  owner_id   uuid        NOT NULL,
  state      text        NOT NULL CHECK (state IN ('draft', 'queued', 'running', 'waiting_input', 'verifying', 'ready_for_review', 'cancelling', 'succeeded', 'failed', 'rejected', 'cancelled')),
  version    bigint      NOT NULL CHECK (version > 0),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, task_id),
  UNIQUE (task_id),
  FOREIGN KEY (tenant_id, owner_id) REFERENCES principal (tenant_id, principal_id)
);

CREATE TABLE attempt (
  tenant_id         uuid        NOT NULL REFERENCES tenant (id),
  attempt_id        uuid        NOT NULL,
  task_id           uuid        NOT NULL,
  number            integer     NOT NULL CHECK (number > 0),
  state             text        NOT NULL CHECK (state IN ('allocated', 'provisioning', 'starting', 'executing', 'waiting_input', 'finalizing', 'cancellation_requested', 'reconciling', 'completed', 'failed', 'terminated')),
  version           bigint      NOT NULL CHECK (version > 0),
  runtime_stopped   boolean     NOT NULL DEFAULT false,
  environment_stopped boolean   NOT NULL DEFAULT false,
  lease_epoch       bigint      NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
  lease_owner       text,
  lease_expires_at  timestamptz,
  created_at        timestamptz NOT NULL,
  updated_at        timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, attempt_id),
  UNIQUE (attempt_id),
  UNIQUE (tenant_id, task_id, number),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  CHECK (NOT (runtime_stopped OR environment_stopped) OR state IN ('cancellation_requested', 'terminated'))
);

CREATE TABLE attention_request (
  tenant_id         uuid        NOT NULL REFERENCES tenant (id),
  attention_id      uuid        NOT NULL,
  task_id           uuid        NOT NULL,
  attempt_id        uuid        NOT NULL,
  kind              text        NOT NULL CHECK (kind IN ('question', 'action_approval', 'authentication_handoff', 'blocked_work')),
  provider_id       uuid,
  operation_id      uuid,
  action_digest     text        CHECK (action_digest IS NULL OR action_digest ~ '^sha256:[0-9a-f]{64}$'),
  policy_generation bigint     NOT NULL CHECK (policy_generation > 0),
  responders        uuid[]      NOT NULL CHECK (cardinality(responders) > 0),
  state             text        NOT NULL CHECK (state IN ('pending', 'decided', 'dispatching', 'delivered', 'consumed', 'expired', 'cancelled', 'superseded')),
  version           bigint      NOT NULL CHECK (version > 0),
  decision_id       uuid,
  expires_at        timestamptz NOT NULL,
  created_at        timestamptz NOT NULL,
  updated_at        timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, attention_id),
  UNIQUE (attention_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  CHECK (expires_at > created_at),
  CHECK ((kind = 'action_approval') = (provider_id IS NOT NULL AND operation_id IS NOT NULL AND action_digest IS NOT NULL))
);

CREATE TABLE decision (
  tenant_id       uuid NOT NULL REFERENCES tenant (id),
  decision_id     uuid NOT NULL,
  attention_id    uuid NOT NULL,
  request_version bigint NOT NULL CHECK (request_version > 0),
  principal_id    uuid NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('approve_once', 'deny', 'answer')),
  answer_digest   text CHECK (answer_digest IS NULL OR answer_digest ~ '^sha256:[0-9a-f]{64}$'),
  decided_at      timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, decision_id),
  UNIQUE (decision_id),
  FOREIGN KEY (tenant_id, attention_id) REFERENCES attention_request (tenant_id, attention_id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal (tenant_id, principal_id),
  CHECK ((kind = 'answer') = (answer_digest IS NOT NULL))
);

CREATE TABLE provider_operation (
  tenant_id  uuid        NOT NULL REFERENCES tenant (id),
  operation_id uuid      NOT NULL,
  attempt_id uuid        NOT NULL,
  provider_id uuid       NOT NULL,
  state      text        NOT NULL CHECK (state IN ('recorded', 'dispatched', 'accepted', 'uncertain', 'succeeded', 'failed', 'rejected')),
  version    bigint      NOT NULL CHECK (version > 0),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, operation_id),
  UNIQUE (operation_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

CREATE TABLE binding (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  attempt_id  uuid        NOT NULL,
  provider_id uuid        NOT NULL,
  operation_id uuid       NOT NULL,
  kind        text        NOT NULL CHECK (kind IN ('runtime_session', 'runtime_turn', 'environment_resource')),
  native_ref  text        NOT NULL CHECK (native_ref <> '' AND char_length(native_ref) <= 256),
  session_ref text        CHECK (session_ref IS NULL OR (session_ref <> '' AND char_length(session_ref) <= 256)),
  bound_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, provider_id, kind, native_ref),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id),
  CHECK ((kind = 'runtime_turn') = (session_ref IS NOT NULL))
);

CREATE TABLE turn_end (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  attempt_id  uuid        NOT NULL,
  provider_id uuid        NOT NULL,
  session_ref text        NOT NULL CHECK (session_ref <> '' AND char_length(session_ref) <= 256),
  turn_ref    text        NOT NULL CHECK (turn_ref <> '' AND char_length(turn_ref) <= 256),
  outcome     text        NOT NULL CHECK (outcome IN ('completed', 'awaiting_input', 'failed', 'interrupted')),
  failure_code text       CHECK (failure_code IS NULL OR failure_code IN ('rate_limited', 'timeout', 'context_exhausted', 'tool_failed', 'provider_error', 'protocol_violation', 'unknown')),
  failure_summary text    CHECK (failure_summary IS NULL OR (failure_summary <> '' AND char_length(failure_summary) <= 512)),
  failure_details_digest text CHECK (failure_details_digest IS NULL OR failure_details_digest ~ '^sha256:[0-9a-f]{64}$'),
  ended_at    timestamptz NOT NULL,
  UNIQUE (tenant_id, provider_id, session_ref, turn_ref),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  CHECK ((outcome = 'failed') = (failure_code IS NOT NULL))
);

CREATE TABLE environment_observation (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  attempt_id   uuid        NOT NULL,
  provider_id  uuid        NOT NULL,
  resource_ref text        NOT NULL CHECK (resource_ref <> '' AND char_length(resource_ref) <= 256),
  state        text        NOT NULL CHECK (state IN ('absent', 'starting', 'running', 'hibernated', 'unknown')),
  sequence     bigint      NOT NULL CHECK (sequence > 0),
  observed_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, provider_id, resource_ref),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

CREATE TABLE stream_cursor (
  tenant_id             uuid   NOT NULL REFERENCES tenant (id),
  attempt_id            uuid   NOT NULL,
  operation_id          uuid   NOT NULL,
  stream                text   NOT NULL CHECK (stream <> '' AND char_length(stream) <= 128 AND stream ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
  generation            bigint NOT NULL CHECK (generation > 0),
  after_updated_sequence bigint NOT NULL CHECK (after_updated_sequence >= 0),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, operation_id, stream),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id)
);

CREATE TABLE authority_grant (
  tenant_id         uuid        NOT NULL REFERENCES tenant (id),
  grant_id          uuid        NOT NULL,
  attempt_id        uuid        NOT NULL,
  provider_id       uuid        NOT NULL,
  action            text        NOT NULL CHECK (action ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$' AND char_length(action) <= 128),
  mode              text        NOT NULL CHECK (mode IN ('allow_once', 'standing')),
  operation_id      uuid,
  input_digest      text        CHECK (input_digest IS NULL OR input_digest ~ '^sha256:[0-9a-f]{64}$'),
  scope_class       text        CHECK (scope_class IS NULL OR scope_class IN ('global', 'setting', 'subject', 'call')),
  scope_target      text        CHECK (scope_target IS NULL OR (scope_target <> '' AND char_length(scope_target) <= 256)),
  granted_by        uuid        NOT NULL,
  policy_generation bigint     NOT NULL CHECK (policy_generation > 0),
  issued_at         timestamptz NOT NULL,
  expires_at        timestamptz NOT NULL,
  revoked_at        timestamptz,
  retired_at        timestamptz,
  PRIMARY KEY (tenant_id, grant_id),
  UNIQUE (grant_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, granted_by) REFERENCES principal (tenant_id, principal_id),
  CHECK (expires_at > issued_at),
  CHECK ((mode = 'allow_once' AND operation_id IS NOT NULL AND input_digest IS NOT NULL AND scope_class IS NULL AND scope_target IS NULL)
      OR (mode = 'standing' AND operation_id IS NULL AND input_digest IS NULL AND retired_at IS NULL AND scope_class IS NOT NULL
          AND ((scope_class IN ('setting', 'subject')) = (scope_target IS NOT NULL)) AND scope_class <> 'call'))
);

CREATE TABLE authority_use (
  tenant_id         uuid        NOT NULL REFERENCES tenant (id),
  operation_id      uuid        NOT NULL,
  attempt_id        uuid        NOT NULL,
  provider_id       uuid        NOT NULL,
  action            text        NOT NULL CHECK (action ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$' AND char_length(action) <= 128),
  input_digest      text        NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
  grant_id          uuid,
  source            text        CHECK (source IS NULL OR source IN ('one_time_approval', 'standing_grant')),
  granted_by        uuid,
  outcome           text        NOT NULL CHECK (outcome IN ('allowed', 'denied')),
  reason            text        CHECK (reason IS NULL OR reason IN ('no_grant', 'cross_tenant', 'wrong_attempt', 'wrong_audience', 'wrong_action', 'wrong_operation', 'input_mismatch', 'scope_mismatch', 'revoked', 'retired', 'not_yet_valid', 'expired', 'stale_generation', 'above_ceiling', 'denied_by_policy')),
  policy_generation bigint     NOT NULL CHECK (policy_generation > 0),
  decided_at        timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, operation_id, decided_at, action),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id),
  CHECK ((grant_id IS NULL) = (source IS NULL)),
  CHECK ((outcome = 'allowed') = (reason IS NULL)),
  CHECK (outcome = 'denied' OR grant_id IS NOT NULL)
);

-- One allowed use per one-time grant: a retired allow-once grant authorizes
-- no second action. Standing grants are unaffected.
CREATE UNIQUE INDEX authority_use_one_time_single_allow
  ON authority_use (grant_id) WHERE outcome = 'allowed' AND source = 'one_time_approval';

CREATE TABLE hook_evidence (
  tenant_id        uuid NOT NULL REFERENCES tenant (id),
  operation_id     uuid NOT NULL,
  outcome          text NOT NULL CHECK (outcome IN ('allowed', 'denied', 'missing', 'timed_out', 'malformed')),
  original_digest  text NOT NULL CHECK (original_digest ~ '^sha256:[0-9a-f]{64}$'),
  effective_digest text CHECK (effective_digest IS NULL OR effective_digest ~ '^sha256:[0-9a-f]{64}$'),
  rewritten        boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, operation_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id),
  CHECK ((outcome = 'allowed') = (effective_digest IS NOT NULL)),
  CHECK (outcome = 'allowed' OR NOT rewritten)
);

CREATE TABLE budget (
  tenant_id  uuid        NOT NULL REFERENCES tenant (id),
  budget_id  uuid        NOT NULL,
  task_id    uuid        NOT NULL,
  unit       text        NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
  ceiling    bigint      NOT NULL CHECK (ceiling > 0 AND ceiling <= 9007199254740991),
  reserved   bigint      NOT NULL CHECK (reserved >= 0 AND reserved <= 9007199254740991),
  consumed   bigint      NOT NULL CHECK (consumed >= 0 AND consumed <= 9007199254740991),
  overshoot  bigint      NOT NULL CHECK (overshoot >= 0 AND overshoot <= 9007199254740991),
  version    bigint      NOT NULL CHECK (version > 0),
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, budget_id),
  UNIQUE (budget_id),
  UNIQUE (tenant_id, task_id, unit),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  CHECK (overshoot >= GREATEST(reserved + consumed - ceiling, 0))
);

CREATE TABLE reservation (
  tenant_id  uuid        NOT NULL REFERENCES tenant (id),
  reservation_id uuid    NOT NULL,
  budget_id  uuid        NOT NULL,
  attempt_id uuid        NOT NULL,
  amount     bigint      NOT NULL CHECK (amount > 0 AND amount <= 9007199254740991),
  consumed   bigint      NOT NULL CHECK (consumed >= 0 AND consumed <= 9007199254740991),
  state      text        NOT NULL CHECK (state IN ('held', 'released')),
  version    bigint      NOT NULL CHECK (version > 0),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, reservation_id),
  UNIQUE (reservation_id),
  FOREIGN KEY (tenant_id, budget_id) REFERENCES budget (tenant_id, budget_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

CREATE TABLE spend_challenge (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  spend_id     uuid        NOT NULL,
  principal_id uuid        NOT NULL,
  task_id      uuid        NOT NULL,
  attempt_id   uuid        NOT NULL,
  operation_id uuid        NOT NULL,
  amount       bigint      NOT NULL CHECK (amount > 0 AND amount <= 9007199254740991),
  unit         text        NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
  nonce        text        NOT NULL CHECK (nonce ~ '^sha256:[0-9a-f]{64}$'),
  state        text        NOT NULL CHECK (state IN ('pending', 'asserted', 'expired')),
  version      bigint      NOT NULL CHECK (version > 0),
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL,
  updated_at   timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, spend_id),
  UNIQUE (spend_id),
  UNIQUE (tenant_id, nonce),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id),
  CHECK (expires_at > created_at)
);

CREATE TABLE spend_receipt (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  spend_id     uuid        NOT NULL PRIMARY KEY REFERENCES spend_challenge (spend_id),
  principal_id uuid        NOT NULL,
  task_id      uuid        NOT NULL,
  attempt_id   uuid        NOT NULL,
  operation_id uuid        NOT NULL,
  amount       bigint      NOT NULL CHECK (amount > 0 AND amount <= 9007199254740991),
  unit         text        NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
  nonce        text        NOT NULL CHECK (nonce ~ '^sha256:[0-9a-f]{64}$'),
  proof        text        NOT NULL CHECK (proof ~ '^sha256:[0-9a-f]{64}$'),
  issued_at    timestamptz NOT NULL,
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id)
);

CREATE TABLE maintenance_claim (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  claim_id     uuid        NOT NULL,
  provider_id  uuid        NOT NULL,
  resource_ref text        NOT NULL CHECK (resource_ref <> '' AND char_length(resource_ref) <= 256),
  state        text        NOT NULL CHECK (state IN ('scheduled', 'claimed', 'completed')),
  claimant     text        CHECK (claimant IS NULL OR (claimant <> '' AND char_length(claimant) <= 256)),
  epoch        bigint      NOT NULL DEFAULT 0 CHECK (epoch >= 0),
  expires_at   timestamptz,
  version      bigint      NOT NULL CHECK (version > 0),
  updated_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, claim_id),
  UNIQUE (claim_id),
  UNIQUE (tenant_id, provider_id, resource_ref),
  CHECK ((state = 'claimed') = (claimant IS NOT NULL)),
  CHECK ((state = 'claimed') = (expires_at IS NOT NULL)),
  CHECK (state <> 'claimed' OR epoch > 0)
);

CREATE TABLE harness_qualification (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  provider_id uuid        NOT NULL,
  mode        text        NOT NULL CHECK (mode IN ('off', 'shadow', 'live')),
  version     bigint      NOT NULL CHECK (version > 0),
  updated_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, provider_id)
);

CREATE TABLE migration_pass (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  migration_id uuid       NOT NULL,
  from_provider_id uuid   NOT NULL,
  to_provider_id uuid     NOT NULL,
  state       text        NOT NULL CHECK (state IN ('planned', 'previewed', 'held', 'executing', 'completed', 'failed', 'cancelled')),
  version     bigint      NOT NULL CHECK (version > 0),
  created_at  timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, migration_id),
  UNIQUE (migration_id),
  CHECK (from_provider_id <> to_provider_id)
);

CREATE TABLE artifact_manifest (
  tenant_id       uuid        NOT NULL REFERENCES tenant (id),
  operation_id    uuid        NOT NULL,
  artifact_id     uuid        NOT NULL,
  task_id         uuid        NOT NULL,
  attempt_id      uuid        NOT NULL,
  producer_id     uuid        NOT NULL,
  kind            text        NOT NULL CHECK (kind IN ('code_change', 'test_result', 'visual_proof', 'external_action', 'agent_claim')),
  digest          text        NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  size_bytes      bigint      NOT NULL CHECK (size_bytes >= 0 AND size_bytes <= 9007199254740991),
  media_type      text        NOT NULL CHECK (media_type <> '' AND char_length(media_type) <= 127 AND position('/' IN media_type) > 1),
  source_revision text        CHECK (source_revision IS NULL OR char_length(source_revision) <= 256),
  writer_epoch    bigint      NOT NULL CHECK (writer_epoch > 0),
  PRIMARY KEY (tenant_id, operation_id),
  UNIQUE (operation_id),
  UNIQUE (tenant_id, artifact_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES provider_operation (tenant_id, operation_id)
);

CREATE TABLE artifact (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  artifact_id  uuid        NOT NULL,
  operation_id uuid        NOT NULL,
  task_id      uuid        NOT NULL,
  attempt_id   uuid        NOT NULL,
  producer_id  uuid        NOT NULL,
  kind         text        NOT NULL CHECK (kind IN ('code_change', 'test_result', 'visual_proof', 'external_action', 'agent_claim')),
  digest       text        NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  size_bytes   bigint      NOT NULL CHECK (size_bytes >= 0 AND size_bytes <= 9007199254740991),
  media_type   text        NOT NULL,
  source_revision text,
  writer_epoch bigint      NOT NULL CHECK (writer_epoch > 0),
  committed_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, artifact_id),
  UNIQUE (artifact_id),
  FOREIGN KEY (tenant_id, operation_id) REFERENCES artifact_manifest (tenant_id, operation_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

CREATE TABLE result_revision (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  revision_id uuid        NOT NULL,
  task_id     uuid        NOT NULL,
  attempt_id  uuid        NOT NULL,
  number      integer     NOT NULL CHECK (number > 0),
  previous_revision_id uuid,
  created_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, revision_id),
  UNIQUE (revision_id),
  UNIQUE (tenant_id, task_id, number),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  CHECK ((number = 1) = (previous_revision_id IS NULL))
);

CREATE TABLE revision_artifact (
  tenant_id   uuid NOT NULL REFERENCES tenant (id),
  revision_id uuid NOT NULL,
  artifact_id uuid NOT NULL,
  PRIMARY KEY (tenant_id, revision_id, artifact_id),
  FOREIGN KEY (tenant_id, revision_id) REFERENCES result_revision (tenant_id, revision_id),
  FOREIGN KEY (tenant_id, artifact_id) REFERENCES artifact (tenant_id, artifact_id)
);

CREATE TABLE result_review (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  review_id   uuid        NOT NULL,
  task_id     uuid        NOT NULL,
  revision_id uuid        NOT NULL,
  reviewer_id uuid        NOT NULL,
  disposition text        NOT NULL CHECK (disposition IN ('accept', 'request_changes', 'reject')),
  task_version bigint     NOT NULL CHECK (task_version > 0),
  reviewed_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, review_id),
  UNIQUE (review_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, revision_id) REFERENCES result_revision (tenant_id, revision_id),
  FOREIGN KEY (tenant_id, reviewer_id) REFERENCES principal (tenant_id, principal_id)
);

CREATE TABLE audit_event (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  audit_id    uuid        NOT NULL,
  record_kind text        NOT NULL CHECK (record_kind IN ('task', 'attempt', 'attention_request', 'provider_operation', 'harness_qualification', 'migration_pass', 'maintenance_claim', 'budget', 'reservation', 'spend_challenge', 'artifact')),
  record_id   uuid        NOT NULL,
  actor_kind  text        NOT NULL CHECK (actor_kind IN ('principal', 'provider', 'system')),
  principal_id uuid,
  provider_id uuid,
  event       text        NOT NULL CHECK (event <> ''),
  from_state  text        NOT NULL CHECK (from_state <> ''),
  to_state    text,
  version     bigint      NOT NULL CHECK (version > 0),
  outcome     text        NOT NULL CHECK (outcome IN ('applied', 'rejected')),
  reason      text        CHECK (reason IS NULL OR reason IN ('version_conflict', 'terminal_state', 'invalid_transition', 'ambiguous_target', 'invalid_target', 'cross_tenant', 'wrong_record', 'unauthorized_actor', 'decision_not_allowed', 'request_expired', 'stop_already_observed', 'stale_claim', 'claim_held', 'stale_policy', 'ceiling_reached', 'assertion_mismatch', 'stale_writer', 'admission_retry', 'not_current_revision')),
  occurred_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, audit_id),
  UNIQUE (audit_id),
  CHECK ((actor_kind = 'principal') = (principal_id IS NOT NULL AND provider_id IS NULL)),
  CHECK ((actor_kind = 'provider') = (principal_id IS NULL AND provider_id IS NOT NULL)),
  CHECK ((actor_kind = 'system') = (principal_id IS NULL AND provider_id IS NULL)),
  CHECK ((outcome = 'applied') = (to_state IS NOT NULL AND reason IS NULL))
);

CREATE TABLE durable_event (
  tenant_id   uuid        NOT NULL REFERENCES tenant (id),
  event_id    uuid        NOT NULL,
  attempt_id  uuid,
  record_kind text        NOT NULL CHECK (record_kind IN ('task', 'attempt', 'attention_request', 'provider_operation', 'harness_qualification', 'migration_pass', 'maintenance_claim', 'budget', 'reservation', 'spend_challenge', 'artifact')),
  record_id   uuid        NOT NULL,
  event       text        NOT NULL CHECK (event <> ''),
  from_state  text        NOT NULL CHECK (from_state <> ''),
  to_state    text,
  version     bigint      NOT NULL CHECK (version > 0),
  occurred_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, event_id),
  UNIQUE (event_id),
  UNIQUE (tenant_id, record_kind, record_id, version),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id)
);

CREATE TABLE outbox (
  tenant_id        uuid        NOT NULL REFERENCES tenant (id),
  outbox_id        uuid        NOT NULL,
  attempt_id       uuid,
  record_kind      text        NOT NULL CHECK (record_kind IN ('task', 'attempt', 'attention_request', 'provider_operation', 'harness_qualification', 'migration_pass', 'maintenance_claim', 'budget', 'reservation', 'spend_challenge', 'artifact')),
  record_id        uuid        NOT NULL,
  event            text        NOT NULL CHECK (event <> ''),
  payload          jsonb       NOT NULL,
  status           text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'done')),
  claimed_at       timestamptz,
  claimed_by       text,
  lease_epoch      bigint      NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
  lease_expires_at timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, outbox_id),
  UNIQUE (outbox_id),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  CHECK ((status = 'pending') = (claimed_at IS NULL AND claimed_by IS NULL))
);

CREATE INDEX outbox_pending ON outbox (created_at) WHERE status = 'pending';

-- Grants are issued only at the tenant's current policy generation
-- (identity.IssueGrant). The check lives here so a stale generation can never
-- be persisted, even if application validation is bypassed.
-- +goose StatementBegin
CREATE FUNCTION check_grant_generation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_generation bigint;
BEGIN
  SELECT generation INTO current_generation FROM policy WHERE tenant_id = NEW.tenant_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'dac_plane: no policy for tenant %', NEW.tenant_id;
  END IF;
  IF NEW.policy_generation != current_generation THEN
    RAISE EXCEPTION 'dac_plane: stale grant generation % (policy is %)', NEW.policy_generation, current_generation;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER authority_grant_generation_check
  BEFORE INSERT ON authority_grant
  FOR EACH ROW EXECUTE FUNCTION check_grant_generation();

-- Append-only records: revisions, reviews, receipts, uses, decisions,
-- manifests, artifacts, evidence and audit/durable events are never updated or
-- deleted in place. Later judgements are new rows (#20 persists them with a
-- version compare-and-set on the referenced record).
-- +goose StatementBegin
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'dac_plane_immutable: % is append-only', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_event_immutable
  BEFORE UPDATE OR DELETE ON audit_event
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER authority_use_immutable
  BEFORE UPDATE OR DELETE ON authority_use
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER decision_immutable
  BEFORE UPDATE OR DELETE ON decision
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER spend_receipt_immutable
  BEFORE UPDATE OR DELETE ON spend_receipt
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER result_revision_immutable
  BEFORE UPDATE OR DELETE ON result_revision
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER revision_artifact_immutable
  BEFORE UPDATE OR DELETE ON revision_artifact
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER result_review_immutable
  BEFORE UPDATE OR DELETE ON result_review
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER artifact_manifest_immutable
  BEFORE UPDATE OR DELETE ON artifact_manifest
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER artifact_immutable
  BEFORE UPDATE OR DELETE ON artifact
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER hook_evidence_immutable
  BEFORE UPDATE OR DELETE ON hook_evidence
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER durable_event_immutable
  BEFORE UPDATE OR DELETE ON durable_event
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS durable_event_immutable ON durable_event;
DROP TRIGGER IF EXISTS hook_evidence_immutable ON hook_evidence;
DROP TRIGGER IF EXISTS artifact_immutable ON artifact;
DROP TRIGGER IF EXISTS artifact_manifest_immutable ON artifact_manifest;
DROP TRIGGER IF EXISTS result_review_immutable ON result_review;
DROP TRIGGER IF EXISTS revision_artifact_immutable ON revision_artifact;
DROP TRIGGER IF EXISTS result_revision_immutable ON result_revision;
DROP TRIGGER IF EXISTS spend_receipt_immutable ON spend_receipt;
DROP TRIGGER IF EXISTS decision_immutable ON decision;
DROP TRIGGER IF EXISTS authority_use_immutable ON authority_use;
DROP TRIGGER IF EXISTS audit_event_immutable ON audit_event;
DROP TRIGGER IF EXISTS authority_grant_generation_check ON authority_grant;
DROP FUNCTION IF EXISTS reject_mutation();
DROP FUNCTION IF EXISTS check_grant_generation();
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS durable_event;
DROP TABLE IF EXISTS audit_event;
DROP TABLE IF EXISTS result_review;
DROP TABLE IF EXISTS revision_artifact;
DROP TABLE IF EXISTS result_revision;
DROP TABLE IF EXISTS artifact;
DROP TABLE IF EXISTS artifact_manifest;
DROP TABLE IF EXISTS migration_pass;
DROP TABLE IF EXISTS harness_qualification;
DROP TABLE IF EXISTS maintenance_claim;
DROP TABLE IF EXISTS spend_receipt;
DROP TABLE IF EXISTS spend_challenge;
DROP TABLE IF EXISTS reservation;
DROP TABLE IF EXISTS budget;
DROP TABLE IF EXISTS hook_evidence;
DROP TABLE IF EXISTS authority_use;
DROP INDEX IF EXISTS authority_use_one_time_single_allow;
DROP TABLE IF EXISTS authority_grant;
DROP TABLE IF EXISTS stream_cursor;
DROP TABLE IF EXISTS environment_observation;
DROP TABLE IF EXISTS turn_end;
DROP TABLE IF EXISTS binding;
DROP TABLE IF EXISTS provider_operation;
DROP TABLE IF EXISTS decision;
DROP TABLE IF EXISTS attention_request;
DROP TABLE IF EXISTS attempt;
DROP TABLE IF EXISTS task;
DROP TABLE IF EXISTS policy;
DROP TABLE IF EXISTS principal;
DROP TABLE IF EXISTS tenant;
