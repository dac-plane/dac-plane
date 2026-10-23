-- +goose Up
-- ADR-0007: an external identity (issuer, subject) becomes a principal only
-- through an explicit, tenant-scoped binding created by trusted provisioning.
CREATE TABLE identity_binding (
  issuer text NOT NULL CHECK (length(issuer) <= 2048 AND issuer ~ '^https://[!-~]+$' AND position('?' in issuer) = 0 AND position('#' in issuer) = 0),
  subject text NOT NULL CHECK (subject ~ '^[!-~]{1,255}$'),
  tenant_id uuid NOT NULL,
  principal_id uuid NOT NULL,
  enabled boolean NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (issuer, subject, tenant_id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal(tenant_id, principal_id)
);

CREATE INDEX identity_binding_principal ON identity_binding (tenant_id, principal_id);

-- The narrow permission to read every task of a tenant; without it a
-- principal reads only the tasks it owns.
CREATE TABLE tenant_reader (
  tenant_id uuid NOT NULL,
  principal_id uuid NOT NULL,
  enabled boolean NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal(tenant_id, principal_id)
);

-- Append-only record of every trusted provisioning change. It names what
-- changed and when; it never holds credential material.
CREATE TABLE provisioning_change (
  change_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  operation text NOT NULL CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader')),
  tenant_id uuid NOT NULL REFERENCES tenant(id),
  principal_id uuid,
  issuer text CHECK (issuer IS NULL OR length(issuer) <= 2048),
  subject text CHECK (subject IS NULL OR subject ~ '^[!-~]{1,255}$'),
  occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK ((operation IN ('bind_identity','unbind_identity')) = (issuer IS NOT NULL AND subject IS NOT NULL)),
  CHECK ((operation = 'create_tenant') = (principal_id IS NULL))
);

CREATE TRIGGER provisioning_change_immutable
  BEFORE UPDATE OR DELETE ON provisioning_change
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
LOCK TABLE identity_binding, tenant_reader, provisioning_change IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM identity_binding)
 OR EXISTS (SELECT 1 FROM tenant_reader)
 OR EXISTS (SELECT 1 FROM provisioning_change) THEN
  RAISE EXCEPTION 'cannot downgrade while version 10 actor identity exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE provisioning_change;
DROP TABLE tenant_reader;
DROP TABLE identity_binding;
