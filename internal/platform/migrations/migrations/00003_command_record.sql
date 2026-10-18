-- +goose Up
-- 00003: idempotent command records for issue #23.
--
-- Additive only; 00001 and 00002 are frozen. A command record binds a
-- client nonce, scoped to its tenant, principal and command kind, to the
-- fingerprint of the request it first carried and to the facts of the
-- outcome it produced. A retry with the same nonce and fingerprint replays
-- that outcome; a different fingerprint is refused. The row is written as a
-- placeholder at the start of the command and filled before it commits, so
-- the primary key serializes concurrent duplicates before any work exists.
-- No command list is enforced here: new command kinds must not need a
-- schema migration.

CREATE TABLE command_record (
  tenant_id      uuid        NOT NULL REFERENCES tenant (id),
  principal_id   uuid        NOT NULL,
  command        text        NOT NULL CHECK (command <> ''),
  nonce          text        NOT NULL CHECK (nonce <> '' AND char_length(nonce) <= 128),
  request_digest text        NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
  result         jsonb       NOT NULL CHECK (jsonb_typeof(result) = 'object'),
  created_at     timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, principal_id, command, nonce),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal (tenant_id, principal_id)
);

-- +goose Down
DROP TABLE IF EXISTS command_record;
