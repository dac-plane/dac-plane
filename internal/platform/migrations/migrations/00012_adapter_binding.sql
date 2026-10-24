-- +goose Up
-- ADR-0008: a provider exists in a tenant only through a registration, and an
-- adapter speaks for it only through an explicit binding of its external
-- identity (issuer, subject). Both are created by trusted provisioning.
CREATE TABLE provider_registration (
  tenant_id uuid NOT NULL REFERENCES tenant(id),
  provider_id uuid NOT NULL,
  role text NOT NULL CHECK (role IN ('runtime','environment','enforcement','artifact_store')),
  artifact_store_id uuid,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, provider_id),
  CHECK (artifact_store_id IS NULL OR (role = 'runtime' AND artifact_store_id <> provider_id)),
  FOREIGN KEY (tenant_id, artifact_store_id) REFERENCES provider_registration(tenant_id, provider_id)
);

-- One identity speaks for exactly one provider, so a connection needs no
-- tenant in its request; a provider may have several bound identities.
CREATE TABLE adapter_binding (
  issuer text NOT NULL CHECK (length(issuer) <= 2048 AND issuer ~ '^https://[!-~]+$' AND position('?' in issuer) = 0 AND position('#' in issuer) = 0),
  subject text NOT NULL CHECK (subject ~ '^[!-~]{1,255}$'),
  tenant_id uuid NOT NULL,
  provider_id uuid NOT NULL,
  enabled boolean NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (issuer, subject),
  FOREIGN KEY (tenant_id, provider_id) REFERENCES provider_registration(tenant_id, provider_id)
);

CREATE INDEX adapter_binding_provider ON adapter_binding (tenant_id, provider_id);

-- Provisioning changes gain the provider operations. The table is
-- append-only, so its checks are replaced rather than its rows rewritten.
ALTER TABLE provisioning_change ADD COLUMN provider_id uuid;
-- +goose StatementBegin
DO $$
DECLARE constraint_name text;
BEGIN
  FOR constraint_name IN
    SELECT conname FROM pg_constraint WHERE conrelid = 'provisioning_change'::regclass AND contype = 'c'
  LOOP
    EXECUTE format('ALTER TABLE provisioning_change DROP CONSTRAINT %I', constraint_name);
  END LOOP;
END $$;
-- +goose StatementEnd
ALTER TABLE provisioning_change
  ADD CONSTRAINT provisioning_change_operation CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader','register_provider','bind_adapter','unbind_adapter')),
  ADD CONSTRAINT provisioning_change_issuer CHECK (issuer IS NULL OR length(issuer) <= 2048),
  ADD CONSTRAINT provisioning_change_subject CHECK (subject IS NULL OR subject ~ '^[!-~]{1,255}$'),
  ADD CONSTRAINT provisioning_change_identity CHECK ((operation IN ('bind_identity','unbind_identity','bind_adapter','unbind_adapter')) = (issuer IS NOT NULL AND subject IS NOT NULL)),
  ADD CONSTRAINT provisioning_change_principal CHECK ((operation IN ('create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader')) = (principal_id IS NOT NULL)),
  ADD CONSTRAINT provisioning_change_provider CHECK ((operation IN ('register_provider','bind_adapter','unbind_adapter')) = (provider_id IS NOT NULL));

-- +goose Down
LOCK TABLE provider_registration, adapter_binding, provisioning_change IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM provider_registration)
 OR EXISTS (SELECT 1 FROM adapter_binding)
 OR EXISTS (SELECT 1 FROM provisioning_change WHERE provider_id IS NOT NULL) THEN
  RAISE EXCEPTION 'cannot downgrade while version 12 adapter registrations exist';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE provisioning_change
  DROP CONSTRAINT provisioning_change_operation,
  DROP CONSTRAINT provisioning_change_issuer,
  DROP CONSTRAINT provisioning_change_subject,
  DROP CONSTRAINT provisioning_change_identity,
  DROP CONSTRAINT provisioning_change_principal,
  DROP CONSTRAINT provisioning_change_provider,
  DROP COLUMN provider_id;
ALTER TABLE provisioning_change
  ADD CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader')),
  ADD CHECK (issuer IS NULL OR length(issuer) <= 2048),
  ADD CHECK (subject IS NULL OR subject ~ '^[!-~]{1,255}$'),
  ADD CHECK ((operation IN ('bind_identity','unbind_identity')) = (issuer IS NOT NULL AND subject IS NOT NULL)),
  ADD CHECK ((operation = 'create_tenant') = (principal_id IS NULL));
DROP TABLE adapter_binding;
DROP TABLE provider_registration;
