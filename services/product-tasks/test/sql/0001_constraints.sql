-- Constraint tests for migration 0001. Run with: psql -v ON_ERROR_STOP=1 -f <this file>
SET search_path = product;
BEGIN;
INSERT INTO tenant VALUES ('ten_aaaaaaaa','policy@1'), ('ten_bbbbbbbb','policy@1');
INSERT INTO principal (id, idp_subject) VALUES ('prn_aaaaaaaa','sub-a'), ('prn_bbbbbbbb','sub-b');
INSERT INTO project (id, tenant_id, trust_group) VALUES ('proj_aaaaaaaa','ten_aaaaaaaa','task');
INSERT INTO task (id, tenant_id, project_id, owner_id, profile_version, objective, acceptance_criteria, state)
  VALUES ('task_aaaaaaaa','ten_aaaaaaaa','proj_aaaaaaaa','prn_aaaaaaaa','engineering@3','Fix login','["tests pass"]','queued');
INSERT INTO attempt (id, tenant_id, task_id, runtime_adapter_version, state)
  VALUES ('att_aaaaaaaa','ten_aaaaaaaa','task_aaaaaaaa','sim@0','allocated');

CREATE FUNCTION pg_temp.expect_fail(sql text, label text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  BEGIN EXECUTE sql; EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'ok: % (%)', label, SQLERRM; RETURN; END;
  RAISE EXCEPTION 'FAIL: expected error for %', label;
END $$;

-- cross-tenant reference rejected
SELECT pg_temp.expect_fail($$INSERT INTO task (id, tenant_id, project_id, owner_id, profile_version, objective, acceptance_criteria, state)
  VALUES ('task_cccccccc','ten_bbbbbbbb','proj_aaaaaaaa','prn_bbbbbbbb','e@1','x','["c"]','queued')$$, 'cross-tenant project FK');
-- acceptance criteria required
SELECT pg_temp.expect_fail($$INSERT INTO task (id, tenant_id, project_id, owner_id, profile_version, objective, acceptance_criteria, state)
  VALUES ('task_dddddddd','ten_aaaaaaaa','proj_aaaaaaaa','prn_aaaaaaaa','e@1','x','[]','queued')$$, 'empty acceptance criteria');
-- one active attempt per task
SELECT pg_temp.expect_fail($$INSERT INTO attempt (id, tenant_id, task_id, runtime_adapter_version, state)
  VALUES ('att_bbbbbbbb','ten_aaaaaaaa','task_aaaaaaaa','sim@0','allocated')$$, 'second active attempt');
-- invalid state
SELECT pg_temp.expect_fail($$UPDATE task SET state='done' WHERE id='task_aaaaaaaa'$$, 'unknown task state');

-- lease fencing: first claim gets epoch 1, stale claim with epoch 0 gets nothing
DO $$ DECLARE e bigint; BEGIN
  SELECT claim_attempt('ten_aaaaaaaa','att_aaaaaaaa','worker-1',0,'90 seconds') INTO e;
  IF e IS DISTINCT FROM 1 THEN RAISE EXCEPTION 'FAIL: claim epoch %', e; END IF;
  SELECT claim_attempt('ten_aaaaaaaa','att_aaaaaaaa','worker-2',0,'90 seconds') INTO e;
  IF e IS NOT NULL THEN RAISE EXCEPTION 'FAIL: stale claim succeeded'; END IF;
  SELECT claim_attempt('ten_aaaaaaaa','att_aaaaaaaa','worker-2',1,'90 seconds') INTO e;
  IF e IS NOT NULL THEN RAISE EXCEPTION 'FAIL: claim while live lease held by other owner'; END IF;
  RAISE NOTICE 'ok: lease fencing';
END $$;

INSERT INTO attention_request (id, tenant_id, task_id, attempt_id, type, native_request_ref, action_hash, payload_ref, status, expiry, policy_version, authorized_responders)
  VALUES ('req_aaaaaaaa','ten_aaaaaaaa','task_aaaaaaaa','att_aaaaaaaa','question','native-1',NULL,'obj://q','pending', now()+interval '1 hour','policy@1','{prn_aaaaaaaa}');
-- one blocking request per attempt
SELECT pg_temp.expect_fail($$INSERT INTO attention_request (id, tenant_id, task_id, attempt_id, type, native_request_ref, payload_ref, status, expiry, policy_version, authorized_responders)
  VALUES ('req_bbbbbbbb','ten_aaaaaaaa','task_aaaaaaaa','att_aaaaaaaa','question','native-2','obj://q','pending', now()+interval '1 hour','policy@1','{prn_aaaaaaaa}')$$, 'second blocking request');
-- approval requires action hash
SELECT pg_temp.expect_fail($$INSERT INTO attention_request (id, tenant_id, task_id, attempt_id, type, native_request_ref, payload_ref, status, blocking, expiry, policy_version, authorized_responders)
  VALUES ('req_cccccccc','ten_aaaaaaaa','task_aaaaaaaa','att_aaaaaaaa','action_approval','native-3','obj://a','pending', false, now()+interval '1 hour','policy@1','{prn_aaaaaaaa}')$$, 'approval without action hash');

-- first decision wins per request version
INSERT INTO decision (id, tenant_id, request_id, request_version, principal_id, decision) VALUES ('dec_aaaaaaaa','ten_aaaaaaaa','req_aaaaaaaa',1,'prn_aaaaaaaa','answer');
SELECT pg_temp.expect_fail($$INSERT INTO decision (id, tenant_id, request_id, request_version, principal_id, decision) VALUES ('dec_bbbbbbbb','ten_aaaaaaaa','req_aaaaaaaa',1,'prn_aaaaaaaa','deny')$$, 'second decision same version');

-- idempotency and outbox dedupe
INSERT INTO idempotency_key (tenant_id, operation, key, request_hash) VALUES ('ten_aaaaaaaa','createTask','k1','h');
SELECT pg_temp.expect_fail($$INSERT INTO idempotency_key (tenant_id, operation, key, request_hash) VALUES ('ten_aaaaaaaa','createTask','k1','h2')$$, 'duplicate idempotency key');
INSERT INTO outbox (command_id, tenant_id, type, target_id, deadline, idempotency_key) VALUES ('cmd_aaaaaaaa','ten_aaaaaaaa','DeliverDecision','req_aaaaaaaa', now()+interval '1 minute','dec_aaaaaaaa');
SELECT pg_temp.expect_fail($$INSERT INTO outbox (command_id, tenant_id, type, target_id, deadline, idempotency_key) VALUES ('cmd_bbbbbbbb','ten_aaaaaaaa','DeliverDecision','req_aaaaaaaa', now()+interval '1 minute','dec_aaaaaaaa')$$, 'duplicate outbox command');

-- per-task event sequence unique; immutable events, artifacts and audit
INSERT INTO event (event_id, tenant_id, task_id, sequence, type, occurred_at, producer, correlation_id, classification)
  VALUES ('evt_aaaaaaaa','ten_aaaaaaaa','task_aaaaaaaa',1,'task.submitted',now(),'api@0','corr','project_internal');
SELECT pg_temp.expect_fail($$INSERT INTO event (event_id, tenant_id, task_id, sequence, type, occurred_at, producer, correlation_id, classification)
  VALUES ('evt_bbbbbbbb','ten_aaaaaaaa','task_aaaaaaaa',1,'task.submitted',now(),'api@0','corr','project_internal')$$, 'duplicate sequence');
SELECT pg_temp.expect_fail($$UPDATE event SET type='x.y'$$, 'event update');
INSERT INTO artifact (id, tenant_id, task_id, attempt_id, uri, digest, classification, source_revision)
  VALUES ('art_aaaaaaaa','ten_aaaaaaaa','task_aaaaaaaa','att_aaaaaaaa','obj://p','sha256:00','project_internal','abc');
SELECT pg_temp.expect_fail($$UPDATE artifact SET digest='sha256:11'$$, 'artifact overwrite');
INSERT INTO audit_log (tenant_id, actor, action, object_type, object_id) VALUES ('ten_aaaaaaaa','prn_aaaaaaaa','task.submit','task','task_aaaaaaaa');
SELECT pg_temp.expect_fail($$DELETE FROM audit_log$$, 'audit delete');

ROLLBACK;
\echo 'ALL SQL CONSTRAINT TESTS PASSED'
