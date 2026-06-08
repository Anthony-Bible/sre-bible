-- +goose Up
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS deadpool_mode BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN IF EXISTS deadpool_mode;
