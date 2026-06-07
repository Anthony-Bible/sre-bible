-- +goose Up
ALTER TABLE sources ADD COLUMN IF NOT EXISTS full_text TEXT;

-- +goose Down
ALTER TABLE sources DROP COLUMN full_text;
