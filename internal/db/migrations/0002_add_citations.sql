-- +goose Up
ALTER TABLE messages ADD COLUMN IF NOT EXISTS citations TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE messages DROP COLUMN citations;
