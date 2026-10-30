-- +goose Up
-- 00015: durable task-attempt environment assignment (#102).
-- Existing Attempts remain unassigned. Only immutable submission results
-- containing an explicit provider and operation, and migration targets with an
-- environment operation, are backfilled.
ALTER TABLE attempt
  ADD COLUMN environment_provider_id uuid,
  ADD COLUMN environment_operation_id uuid;

UPDATE attempt AS a
SET (environment_provider_id, environment_operation_id) = (
  SELECT env_provider::uuid, env_operation::uuid
  FROM (
    SELECT
      coalesce(c.result->>'environment_provider_id', c.result->'scheduling'->>'environment_provider_id') AS env_provider,
      coalesce(c.result->>'environment_operation_id', c.result->'scheduling'->>'environment_operation_id') AS env_operation
    FROM command_record AS c
    WHERE c.tenant_id = a.tenant_id
      AND c.command IN ('submit_task', 'review_result')
      AND coalesce(c.result->>'attempt', c.result->'scheduling'->>'attempt') = a.attempt_id::text
    ORDER BY c.created_at DESC
    LIMIT 1
  ) AS assignment
  WHERE env_provider ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND env_operation ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
)
WHERE EXISTS (
  SELECT 1 FROM command_record AS c
  WHERE c.tenant_id = a.tenant_id
    AND c.command IN ('submit_task', 'review_result')
    AND coalesce(c.result->>'attempt', c.result->'scheduling'->>'attempt') = a.attempt_id::text
    AND coalesce(c.result->>'environment_provider_id', c.result->'scheduling'->>'environment_provider_id') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND coalesce(c.result->>'environment_operation_id', c.result->'scheduling'->>'environment_operation_id') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
);

UPDATE attempt AS a
SET environment_provider_id = o.provider_id, environment_operation_id = m.environment_operation_id
FROM migration_item AS m
JOIN provider_operation AS o
  ON o.tenant_id = m.tenant_id AND o.operation_id = m.environment_operation_id
WHERE m.tenant_id = a.tenant_id AND m.target_attempt_id = a.attempt_id
  AND a.environment_provider_id IS NULL;

ALTER TABLE attempt
  ADD CONSTRAINT attempt_environment_assignment_pair
  CHECK ((environment_provider_id IS NULL) = (environment_operation_id IS NULL));

CREATE INDEX attempt_environment_assignment
  ON attempt (tenant_id, environment_provider_id, environment_operation_id)
  WHERE environment_provider_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS attempt_environment_assignment;
ALTER TABLE attempt DROP CONSTRAINT IF EXISTS attempt_environment_assignment_pair;
ALTER TABLE attempt
  DROP COLUMN IF EXISTS environment_operation_id,
  DROP COLUMN IF EXISTS environment_provider_id;
