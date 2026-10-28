-- +goose Up
-- 00014: product-owned task execution briefs (#99).
--
-- Columns remain nullable for truthful legacy tasks. The compatible binary
-- stores new briefs in command results while serving schema 13; this migration
-- backfills only those observed values and never invents an objective.

CREATE FUNCTION dac_valid_execution_brief(objective text, criteria jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT
    (objective IS NULL AND criteria IS NULL)
    OR
    (objective IS NOT NULL
	  AND criteria IS NOT NULL
      AND btrim(objective) <> ''
      AND char_length(objective) <= 10000
      AND jsonb_typeof(criteria) = 'array'
      AND jsonb_array_length(criteria) BETWEEN 1 AND 32
      AND NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements(criteria) AS item(value)
        WHERE jsonb_typeof(value) <> 'string'
           OR btrim(value #>> '{}') = ''
           OR char_length(value #>> '{}') > 1000
      ))
$$;

ALTER TABLE task
  ADD COLUMN objective text,
  ADD COLUMN acceptance_criteria jsonb;

UPDATE task AS t
SET objective = c.result->>'objective',
    acceptance_criteria = c.result->'acceptance_criteria'
FROM command_record AS c
WHERE c.tenant_id = t.tenant_id
  AND c.command = 'submit_task'
  AND c.result->>'task' = t.task_id::text
  AND c.result ? 'objective'
  AND c.result ? 'acceptance_criteria'
  AND dac_valid_execution_brief(c.result->>'objective', c.result->'acceptance_criteria');

ALTER TABLE task
  ADD CONSTRAINT task_execution_brief_valid
  CHECK (dac_valid_execution_brief(objective, acceptance_criteria));

-- +goose Down
ALTER TABLE task DROP CONSTRAINT IF EXISTS task_execution_brief_valid;
ALTER TABLE task DROP COLUMN IF EXISTS acceptance_criteria;
ALTER TABLE task DROP COLUMN IF EXISTS objective;
DROP FUNCTION IF EXISTS dac_valid_execution_brief(text, jsonb);
