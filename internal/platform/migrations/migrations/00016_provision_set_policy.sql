-- +goose Up
-- Trusted provisioning gains setting a tenant's authority policy (#115), so no
-- deployment procedure updates the policy table directly. Additive: version 15
-- keeps serving.
ALTER TABLE provisioning_change DROP CONSTRAINT provisioning_change_operation;
ALTER TABLE provisioning_change
  ADD CONSTRAINT provisioning_change_operation CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader','register_provider','bind_adapter','unbind_adapter','qualify_harness','set_policy'));

-- +goose Down
LOCK TABLE provisioning_change IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM provisioning_change WHERE operation = 'set_policy') THEN
  RAISE EXCEPTION 'cannot downgrade while version 16 policy changes exist';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE provisioning_change DROP CONSTRAINT provisioning_change_operation;
ALTER TABLE provisioning_change
  ADD CONSTRAINT provisioning_change_operation CHECK (operation IN ('create_tenant','create_principal','bind_identity','unbind_identity','grant_reader','revoke_reader','register_provider','bind_adapter','unbind_adapter','qualify_harness'));
