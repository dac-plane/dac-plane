-- +goose Up
-- 00020: what a surface needs to list tasks and to name providers (ADR-0028,
-- #155, #156).
--
-- Additive: version 19 keeps serving. The index serves the task listing, newest
-- first by creation; the listing works without it, only more slowly. A
-- registration may carry a name a person recognises, written by trusted
-- provisioning.

ALTER TABLE provider_registration
  ADD COLUMN display_name text CHECK (display_name IS NULL OR dac_valid_line(display_name, 100));

CREATE INDEX task_listing ON task (tenant_id, created_at DESC, task_id DESC);

-- +goose Down
DROP INDEX task_listing;
ALTER TABLE provider_registration DROP COLUMN display_name;
