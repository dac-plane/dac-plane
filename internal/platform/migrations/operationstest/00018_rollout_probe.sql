-- +goose Up
-- Operations-only additive probe for the #74 old/new-image rollout. This
-- file is excluded from the release migration source by Go build constraints.
CREATE TABLE operations_rollout_probe (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  migrated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO operations_rollout_probe (singleton) VALUES (true);

-- +goose Down
DROP TABLE operations_rollout_probe;
