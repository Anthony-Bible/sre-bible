-- +goose Up
ALTER TABLE messages ADD COLUMN IF NOT EXISTS trace JSONB;

-- +goose Down
ALTER TABLE messages DROP COLUMN IF EXISTS trace;
