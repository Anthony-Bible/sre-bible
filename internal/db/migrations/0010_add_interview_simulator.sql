-- +goose Up
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS interview_active BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS interview_state JSONB;

-- +goose Down
ALTER TABLE sessions DROP COLUMN IF EXISTS interview_state;
ALTER TABLE sessions DROP COLUMN IF EXISTS interview_active;
