-- +goose Up
-- Trusted provisioning gains the initial harness qualification, the seed a
-- deployed tenant needs before dispatch (ADR-0009, #73). Later moves stay
-- with the fenced migration passes. Additive: version 12 keeps serving.
ALTER TABLE provisioning_change
  DROP CONSTRAINT provisioning_change_operation,
  DROP CONSTRAINT provisioning_change_provider;
ALTER TABLE provisioning_change
  ADD CONSTRAINT provisioning_change_operation CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader','register_provider','bind_adapter','unbind_adapter','qualify_harness')),
  ADD CONSTRAINT provisioning_change_provider CHECK ((operation IN ('register_provider','bind_adapter','unbind_adapter','qualify_harness')) = (provider_id IS NOT NULL));

-- +goose Down
LOCK TABLE provisioning_change IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM provisioning_change WHERE operation = 'qualify_harness') THEN
  RAISE EXCEPTION 'cannot downgrade while version 13 harness qualifications exist';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE provisioning_change
  DROP CONSTRAINT provisioning_change_operation,
  DROP CONSTRAINT provisioning_change_provider;
ALTER TABLE provisioning_change
  ADD CONSTRAINT provisioning_change_operation CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader','register_provider','bind_adapter','unbind_adapter')),
  ADD CONSTRAINT provisioning_change_provider CHECK ((operation IN ('register_provider','bind_adapter','unbind_adapter')) = (provider_id IS NOT NULL));
