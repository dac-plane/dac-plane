-- +goose Up
-- 00004: attention native correlation and attempt-scoped lookups for #24.
--
-- Additive only; 00001-00003 are frozen. attention_native binds the native
-- request reference a provider raised to the product attention request, under
-- the provider that reported it: ingestion resolves provider-native keys and
-- never trusts them as product identity. For an action approval the row also
-- carries the governed action class the observation named, the provenance a
-- later decision needs to derive its one-time grant.

CREATE TABLE attention_native (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  provider_id  uuid        NOT NULL,
  native_ref   text        NOT NULL CHECK (native_ref <> '' AND char_length(native_ref) <= 256),
  attention_id uuid        NOT NULL,
  action_class text        CHECK (action_class IS NULL OR (action_class ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$' AND char_length(action_class) <= 128)),
  created_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, provider_id, native_ref),
  UNIQUE (tenant_id, attention_id),
  FOREIGN KEY (tenant_id, attention_id) REFERENCES attention_request (tenant_id, attention_id)
);

-- One blocking request per attempt gates the wait state the boundary raises;
-- grants and dispatched operations resolve by attempt when a wait is decided
-- or cancelled.
CREATE INDEX attention_request_attempt ON attention_request (tenant_id, attempt_id);
CREATE INDEX provider_operation_attempt ON provider_operation (tenant_id, attempt_id);
CREATE INDEX authority_grant_attempt ON authority_grant (tenant_id, attempt_id);

-- +goose Down
DROP INDEX IF EXISTS authority_grant_attempt;
DROP INDEX IF EXISTS provider_operation_attempt;
DROP INDEX IF EXISTS attention_request_attempt;
DROP TABLE IF EXISTS attention_native;
