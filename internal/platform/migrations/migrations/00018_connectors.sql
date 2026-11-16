-- +goose Up
-- 00018: connections, their grants and connector calls (ADR-0022, #137).
--
-- Additive: version 17 keeps serving. A connection refers to a credential its
-- connector adapter holds; no column here can hold one. A grant lets an agent's
-- or a task's work call some of a connection's tools. A connector call is the
-- gateway's record of one tool call: its authority, its exact input and, once
-- the connector answers, its output for the runtime that asked.

ALTER TABLE provider_registration DROP CONSTRAINT provider_registration_role_check;
ALTER TABLE provider_registration ADD CONSTRAINT provider_registration_role_check
  CHECK (role IN ('runtime','environment','enforcement','artifact_store','connector'));

CREATE FUNCTION dac_valid_line(value text, max_length integer)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT btrim(value) <> '' AND value !~ '[\n\r]' AND char_length(value) <= max_length
$$;

CREATE TABLE connection (
  tenant_id      uuid        NOT NULL REFERENCES tenant (id),
  connection_id  uuid        NOT NULL,
  provider_id    uuid        NOT NULL,
  connector_role text        NOT NULL GENERATED ALWAYS AS ('connector') STORED,
  owner_id       uuid        NOT NULL,
  name           text        NOT NULL CHECK (name ~ '^\S([^\n\r]*\S)?$' AND char_length(name) <= 100),
  state          text        NOT NULL CHECK (state IN ('authorizing', 'active', 'removing', 'removed')),
  account        text        CHECK (account IS NULL OR dac_valid_line(account, 200)),
  permissions    jsonb       CHECK (permissions IS NULL OR (jsonb_typeof(permissions) = 'array' AND jsonb_array_length(permissions) <= 64)),
  version        bigint      NOT NULL CHECK (version > 0),
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, connection_id),
  UNIQUE (connection_id),
  CHECK ((account IS NULL) = (permissions IS NULL)),
  CHECK (state <> 'active' OR account IS NOT NULL),
  FOREIGN KEY (tenant_id, owner_id) REFERENCES principal (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, provider_id, connector_role) REFERENCES provider_registration (tenant_id, provider_id, role)
);

CREATE TABLE connection_grant (
  tenant_id     uuid        NOT NULL,
  grant_id      uuid        NOT NULL,
  connection_id uuid        NOT NULL,
  agent_id      uuid,
  task_id       uuid,
  tools         jsonb       NOT NULL CHECK (jsonb_typeof(tools) = 'array' AND jsonb_array_length(tools) BETWEEN 1 AND 64),
  permission    text        NOT NULL CHECK (permission IN ('ask', 'always')),
  call_limit    integer     NOT NULL CHECK (call_limit BETWEEN 1 AND 10000),
  state         text        NOT NULL CHECK (state IN ('active', 'revoked')),
  granted_by    uuid        NOT NULL,
  version       bigint      NOT NULL CHECK (version > 0),
  expires_at    timestamptz NOT NULL,
  created_at    timestamptz NOT NULL,
  updated_at    timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, grant_id),
  UNIQUE (grant_id),
  CHECK ((agent_id IS NULL) <> (task_id IS NULL)),
  CHECK (expires_at > created_at),
  FOREIGN KEY (tenant_id, connection_id) REFERENCES connection (tenant_id, connection_id),
  FOREIGN KEY (tenant_id, agent_id) REFERENCES agent (tenant_id, agent_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, granted_by) REFERENCES principal (tenant_id, principal_id)
);

CREATE INDEX connection_grant_connection ON connection_grant (tenant_id, connection_id);
CREATE INDEX connection_grant_agent ON connection_grant (tenant_id, agent_id) WHERE agent_id IS NOT NULL;
CREATE INDEX connection_grant_task ON connection_grant (tenant_id, task_id) WHERE task_id IS NOT NULL;

-- A connector call is created when the gateway decides it. A call waiting for
-- approval has no provider operation yet; an authorized one has, and the
-- connector's result is recorded once. A denied call keeps its record.
CREATE TABLE connector_call (
  tenant_id           uuid        NOT NULL,
  operation_id        uuid        NOT NULL,
  attempt_id          uuid        NOT NULL,
  task_id             uuid        NOT NULL,
  connection_id       uuid        NOT NULL,
  connection_grant_id uuid        NOT NULL,
  provider_id         uuid        NOT NULL,
  tool                text        NOT NULL CHECK (tool ~ '^[a-z][a-z0-9_]{0,47}$'),
  input               text        NOT NULL CHECK (octet_length(input) <= 32768),
  input_digest        text        NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
  state               text        NOT NULL CHECK (state IN ('awaiting_approval', 'denied', 'authorized')),
  -- Text, not jsonb: digests cover the exact bytes, which jsonb would rewrite.
  execution           text        CHECK (execution IS NULL OR octet_length(execution) <= 65536),
  output              text        CHECK (output IS NULL OR octet_length(output) <= 262144),
  output_digest       text        CHECK (output_digest IS NULL OR output_digest ~ '^sha256:[0-9a-f]{64}$'),
  is_error            boolean,
  completed_at        timestamptz,
  created_at          timestamptz NOT NULL,
  updated_at          timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, operation_id),
  UNIQUE (operation_id),
  CHECK ((state = 'authorized') = (execution IS NOT NULL)),
  CHECK ((output IS NULL) = (completed_at IS NULL) AND (output IS NULL) = (output_digest IS NULL) AND (output IS NULL) = (is_error IS NULL)),
  CHECK (output IS NULL OR state = 'authorized'),
  FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempt (tenant_id, attempt_id),
  FOREIGN KEY (tenant_id, task_id) REFERENCES task (tenant_id, task_id),
  FOREIGN KEY (tenant_id, connection_grant_id) REFERENCES connection_grant (tenant_id, grant_id),
  FOREIGN KEY (tenant_id, connection_id) REFERENCES connection (tenant_id, connection_id)
);

-- The call limit counts a task's authorized calls under one grant, and a
-- retried call finds its earlier record by its exact input.
CREATE INDEX connector_call_task_grant ON connector_call (tenant_id, task_id, connection_grant_id) WHERE state = 'authorized';
CREATE INDEX connector_call_attempt_input ON connector_call (tenant_id, attempt_id, connection_id, tool, input_digest);

ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent','connection','connection_grant'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent','connection','connection_grant'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent','connection','connection_grant'));

-- +goose Down
LOCK TABLE connection, connection_grant, connector_call, provider_registration, audit_event, durable_event, outbox IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM connection) OR EXISTS (SELECT 1 FROM provider_registration WHERE role = 'connector') THEN
  RAISE EXCEPTION 'cannot downgrade while version 18 connectors exist';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));
DROP TABLE connector_call;
DROP TABLE connection_grant;
DROP TABLE connection;
DROP FUNCTION dac_valid_line(text, integer);
ALTER TABLE provider_registration DROP CONSTRAINT provider_registration_role_check;
ALTER TABLE provider_registration ADD CONSTRAINT provider_registration_role_check
  CHECK (role IN ('runtime','environment','enforcement','artifact_store'));
