-- +goose Up
ALTER TABLE attempt ADD COLUMN artifact_writer_epoch bigint NOT NULL DEFAULT 0 CHECK (artifact_writer_epoch >= 0);
CREATE TABLE result_proposal (
 tenant_id uuid NOT NULL,
 proposal_id uuid NOT NULL,
 task_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 provider_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 writer_epoch bigint NOT NULL CHECK (writer_epoch > 0),
 payload jsonb NOT NULL CHECK (jsonb_typeof(payload)='object'),
 created_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,proposal_id),
 UNIQUE (tenant_id,attempt_id),
 FOREIGN KEY (tenant_id,task_id) REFERENCES task(tenant_id,task_id),
 FOREIGN KEY (tenant_id,attempt_id) REFERENCES attempt(tenant_id,attempt_id),
 FOREIGN KEY (tenant_id,operation_id) REFERENCES provider_operation(tenant_id,operation_id)
);
CREATE TABLE artifact_work (
 tenant_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 proposal_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 manifest jsonb NOT NULL,
 native_ref text NOT NULL,
 authorize_id uuid NOT NULL,
 export_id uuid NOT NULL,
 stat_id uuid NOT NULL,
 store_id uuid,
 deadline timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,operation_id),
 UNIQUE (operation_id),
 UNIQUE (tenant_id,proposal_id,native_ref),
 FOREIGN KEY (tenant_id,proposal_id) REFERENCES result_proposal(tenant_id,proposal_id),
 FOREIGN KEY (tenant_id,attempt_id) REFERENCES attempt(tenant_id,attempt_id)
);
CREATE TABLE artifact_stage_operation (
 tenant_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 work_id uuid NOT NULL,
 method text NOT NULL CHECK (method IN ('authorize_upload','export_artifacts','stat_object','commit_manifest')),
 PRIMARY KEY (tenant_id,operation_id),
 FOREIGN KEY (tenant_id,work_id) REFERENCES artifact_work(tenant_id,operation_id)
);
CREATE TABLE artifact_commit_evidence (
 tenant_id uuid NOT NULL,
 evidence_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 receipt jsonb NOT NULL,
 observed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,evidence_id),
 FOREIGN KEY (tenant_id,operation_id) REFERENCES artifact_work(tenant_id,operation_id)
);
CREATE TABLE artifact_verification (
 tenant_id uuid NOT NULL,
 verification_id uuid NOT NULL,
 artifact_id uuid NOT NULL,
 evidence jsonb NOT NULL,
 PRIMARY KEY (tenant_id,verification_id),
 UNIQUE (tenant_id,artifact_id),
 FOREIGN KEY (tenant_id,artifact_id) REFERENCES artifact(tenant_id,artifact_id)
);
CREATE TABLE revision_verification (
 tenant_id uuid NOT NULL,
 revision_id uuid NOT NULL,
 verification_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,revision_id,verification_id),
 FOREIGN KEY (tenant_id,revision_id) REFERENCES result_revision(tenant_id,revision_id),
 FOREIGN KEY (tenant_id,verification_id) REFERENCES artifact_verification(tenant_id,verification_id)
);
CREATE TRIGGER result_proposal_immutable BEFORE UPDATE OR DELETE ON result_proposal FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER artifact_stage_operation_immutable BEFORE UPDATE OR DELETE ON artifact_stage_operation FOR EACH ROW EXECUTE FUNCTION reject_mutation();
-- Only first-time store routing may change on a staged manifest.
-- +goose StatementBegin
CREATE FUNCTION protect_artifact_work() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'artifact work is immutable'; END IF;
 IF (to_jsonb(NEW)-'store_id') IS DISTINCT FROM (to_jsonb(OLD)-'store_id')
 OR (OLD.store_id IS NOT NULL AND NEW.store_id IS DISTINCT FROM OLD.store_id) THEN
 RAISE EXCEPTION 'artifact work identity is immutable';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER artifact_work_immutable BEFORE UPDATE OR DELETE ON artifact_work FOR EACH ROW EXECUTE FUNCTION protect_artifact_work();
CREATE TRIGGER artifact_commit_evidence_immutable BEFORE UPDATE OR DELETE ON artifact_commit_evidence FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER artifact_verification_immutable BEFORE UPDATE OR DELETE ON artifact_verification FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER revision_verification_immutable BEFORE UPDATE OR DELETE ON revision_verification FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
LOCK TABLE result_proposal,artifact_work,artifact_stage_operation,artifact_commit_evidence,artifact_verification,revision_verification,attempt IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM result_proposal) OR EXISTS (SELECT 1 FROM artifact_work)
 OR EXISTS (SELECT 1 FROM artifact_stage_operation) OR EXISTS (SELECT 1 FROM artifact_commit_evidence)
 OR EXISTS (SELECT 1 FROM artifact_verification) OR EXISTS (SELECT 1 FROM revision_verification)
 OR EXISTS (SELECT 1 FROM attempt WHERE artifact_writer_epoch <> 0) THEN
 RAISE EXCEPTION 'cannot downgrade while version 6 artifact evidence exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE revision_verification;
DROP TABLE artifact_verification;
DROP TABLE artifact_commit_evidence;
DROP TABLE artifact_stage_operation;
DROP TABLE artifact_work;
DROP FUNCTION protect_artifact_work();
DROP TABLE result_proposal;
ALTER TABLE attempt DROP COLUMN artifact_writer_epoch;
