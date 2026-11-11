-- +goose Up
-- 00017: agents, the records tasks are delegated to (ADR-0020, #136).
--
-- Additive: version 16 keeps serving. A task delegated to an agent pins the
-- revision current at submission, so revisions are append-only and a task's
-- reference is checked against them. Both default providers must be
-- registered in their roles, as a submission's providers are.

CREATE FUNCTION dac_valid_agent_profile(name text, role text, instructions text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT
    name ~ '^\S([^\n\r]*\S)?$' AND char_length(name) <= 100
    AND btrim(role) <> '' AND char_length(role) <= 1000
    AND (instructions IS NULL OR (btrim(instructions) <> '' AND char_length(instructions) <= 20000))
$$;

CREATE TABLE agent (
  tenant_id               uuid        NOT NULL REFERENCES tenant (id),
  agent_id                uuid        NOT NULL,
  created_by              uuid        NOT NULL,
  name                    text        NOT NULL,
  role                    text        NOT NULL,
  instructions            text,
  provider_id             uuid        NOT NULL,
  environment_provider_id uuid        NOT NULL,
  runtime_role            text        NOT NULL GENERATED ALWAYS AS ('runtime') STORED,
  environment_role        text        NOT NULL GENERATED ALWAYS AS ('environment') STORED,
  revision                integer     NOT NULL CHECK (revision > 0),
  state                   text        NOT NULL CHECK (state IN ('active', 'retired')),
  version                 bigint      NOT NULL CHECK (version > 0 AND version >= revision),
  created_at              timestamptz NOT NULL,
  updated_at              timestamptz NOT NULL CHECK (updated_at >= created_at),
  PRIMARY KEY (tenant_id, agent_id),
  UNIQUE (agent_id),
  CHECK (dac_valid_agent_profile(name, role, instructions)),
  FOREIGN KEY (tenant_id, created_by) REFERENCES principal (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, provider_id, runtime_role) REFERENCES provider_registration (tenant_id, provider_id, role),
  FOREIGN KEY (tenant_id, environment_provider_id, environment_role) REFERENCES provider_registration (tenant_id, provider_id, role)
);

-- A name identifies one active agent of a tenant; a retired agent frees it.
CREATE UNIQUE INDEX agent_active_name ON agent (tenant_id, name) WHERE state = 'active';

CREATE TABLE agent_revision (
  tenant_id               uuid        NOT NULL,
  agent_id                uuid        NOT NULL,
  revision                integer     NOT NULL CHECK (revision > 0),
  name                    text        NOT NULL,
  role                    text        NOT NULL,
  instructions            text,
  provider_id             uuid        NOT NULL,
  environment_provider_id uuid        NOT NULL,
  created_by              uuid        NOT NULL,
  created_at              timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, agent_id, revision),
  CHECK (dac_valid_agent_profile(name, role, instructions)),
  FOREIGN KEY (tenant_id, agent_id) REFERENCES agent (tenant_id, agent_id),
  FOREIGN KEY (tenant_id, created_by) REFERENCES principal (tenant_id, principal_id)
);

CREATE TRIGGER agent_revision_immutable
  BEFORE UPDATE OR DELETE ON agent_revision
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

ALTER TABLE task
  ADD COLUMN agent_id uuid,
  ADD COLUMN agent_revision integer,
  ADD CONSTRAINT task_agent_pair CHECK ((agent_id IS NULL) = (agent_revision IS NULL)),
  ADD CONSTRAINT task_agent_revision FOREIGN KEY (tenant_id, agent_id, agent_revision)
    REFERENCES agent_revision (tenant_id, agent_id, revision);

CREATE INDEX task_agent ON task (tenant_id, agent_id) WHERE agent_id IS NOT NULL;

ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant','agent'));

-- +goose Down
LOCK TABLE agent, agent_revision, task, audit_event, durable_event, outbox IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM agent) THEN
  RAISE EXCEPTION 'cannot downgrade while version 17 agents exist';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE audit_event DROP CONSTRAINT audit_event_record_kind_check;
ALTER TABLE durable_event DROP CONSTRAINT durable_event_record_kind_check;
ALTER TABLE outbox DROP CONSTRAINT outbox_record_kind_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));
ALTER TABLE durable_event ADD CONSTRAINT durable_event_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));
ALTER TABLE outbox ADD CONSTRAINT outbox_record_kind_check CHECK (record_kind IN ('task','attempt','attention_request','provider_operation','harness_qualification','migration_pass','maintenance_claim','budget','reservation','spend_challenge','artifact','authority_use','authority_grant'));
DROP INDEX task_agent;
ALTER TABLE task
  DROP CONSTRAINT task_agent_revision,
  DROP CONSTRAINT task_agent_pair,
  DROP COLUMN agent_revision,
  DROP COLUMN agent_id;
DROP TRIGGER agent_revision_immutable ON agent_revision;
DROP TABLE agent_revision;
DROP TABLE agent;
DROP FUNCTION dac_valid_agent_profile(text, text, text);
