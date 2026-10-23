-- +goose Up
-- ADR-0007 event streams, sequenced in commit order.
--
-- A plain sequence orders events by insert time, but transactions commit in
-- another order, so a reader that trusts it can skip an event that commits
-- late. Here each event is positioned at COMMIT instead: a deferred
-- constraint trigger takes a per-tenant transaction advisory lock and then
-- draws the next position. The lock is held until the commit is published
-- (PostgreSQL makes a commit visible before it releases the committing
-- transaction's locks), so within a tenant positions become visible strictly
-- in order and `position > cursor` never skips. The lock is the last thing a
-- transaction takes, after all of its other work, so it adds no lock-order
-- inversion; it serializes only the commit tails of one tenant's writers.
--
-- Positions live in their own append-only table, so durable_event stays
-- immutable. Each position also records the task the event belongs to, or
-- NULL for records outside any task. The per-record version on
-- durable_event remains the gapless revision of each record.
CREATE TABLE event_position (
  tenant_id uuid NOT NULL,
  position bigint NOT NULL,
  event_id uuid NOT NULL,
  task_id uuid,
  PRIMARY KEY (tenant_id, position),
  UNIQUE (event_id),
  FOREIGN KEY (tenant_id, event_id) REFERENCES durable_event (tenant_id, event_id)
);
CREATE INDEX event_position_task ON event_position (tenant_id, task_id, position) WHERE task_id IS NOT NULL;
CREATE SEQUENCE event_position_seq AS bigint;

CREATE TRIGGER event_position_immutable
  BEFORE UPDATE OR DELETE ON event_position
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- durable_event_task names the task an event belongs to. It only reads rows
-- the writing transaction already wrote.
-- +goose StatementBegin
CREATE FUNCTION durable_event_task(tenant uuid, kind text, record uuid) RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT CASE kind
    WHEN 'task' THEN record
    WHEN 'attempt' THEN (SELECT task_id FROM attempt WHERE tenant_id = tenant AND attempt_id = record)
    WHEN 'attention_request' THEN (SELECT task_id FROM attention_request WHERE tenant_id = tenant AND attention_id = record)
    WHEN 'provider_operation' THEN (SELECT a.task_id FROM provider_operation o JOIN attempt a ON a.tenant_id = o.tenant_id AND a.attempt_id = o.attempt_id WHERE o.tenant_id = tenant AND o.operation_id = record)
    WHEN 'budget' THEN (SELECT task_id FROM budget WHERE tenant_id = tenant AND budget_id = record)
    WHEN 'reservation' THEN (SELECT a.task_id FROM reservation r JOIN attempt a ON a.tenant_id = r.tenant_id AND a.attempt_id = r.attempt_id WHERE r.tenant_id = tenant AND r.reservation_id = record)
    WHEN 'spend_challenge' THEN (SELECT task_id FROM spend_challenge WHERE tenant_id = tenant AND spend_id = record)
    WHEN 'artifact' THEN (SELECT task_id FROM artifact WHERE tenant_id = tenant AND artifact_id = record)
    WHEN 'authority_grant' THEN (SELECT a.task_id FROM authority_grant g JOIN attempt a ON a.tenant_id = g.tenant_id AND a.attempt_id = g.attempt_id WHERE g.tenant_id = tenant AND g.grant_id = record)
    -- An authority use is recorded against the operation it authorized.
    WHEN 'authority_use' THEN (SELECT a.task_id FROM provider_operation o JOIN attempt a ON a.tenant_id = o.tenant_id AND a.attempt_id = o.attempt_id WHERE o.tenant_id = tenant AND o.operation_id = record)
  END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION durable_event_position() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- Namespace 1 keeps this key apart from every other advisory lock.
  PERFORM pg_advisory_xact_lock(1, hashtext(NEW.tenant_id::text));
  INSERT INTO event_position (tenant_id, position, event_id, task_id)
  VALUES (NEW.tenant_id, nextval('event_position_seq'), NEW.event_id,
          durable_event_task(NEW.tenant_id, NEW.record_kind, NEW.record_id));
  RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER durable_event_position
  AFTER INSERT ON durable_event
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION durable_event_position();

-- Events written before this version are positioned in the order they
-- occurred.
INSERT INTO event_position (tenant_id, position, event_id, task_id)
SELECT tenant_id, nextval('event_position_seq'), event_id, durable_event_task(tenant_id, record_kind, record_id)
FROM (SELECT * FROM durable_event ORDER BY occurred_at, event_id) existing;

-- The stream generation. A cursor from another generation is reset; the
-- restore runbook advances it so a cursor beyond a restored head is caught.
CREATE TABLE event_stream (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  generation bigint NOT NULL CHECK (generation >= 1)
);
INSERT INTO event_stream (generation) VALUES (1);

-- +goose Down
LOCK TABLE durable_event, event_position, event_stream IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
 IF (SELECT generation FROM event_stream) <> 1 THEN
  RAISE EXCEPTION 'cannot downgrade while version 11 event stream generation has advanced';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE event_stream;
DROP TRIGGER durable_event_position ON durable_event;
DROP FUNCTION durable_event_position();
DROP FUNCTION durable_event_task(uuid, text, uuid);
DROP TABLE event_position;
DROP SEQUENCE event_position_seq;
