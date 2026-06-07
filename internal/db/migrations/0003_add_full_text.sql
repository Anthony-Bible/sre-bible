-- +goose Up
ALTER TABLE sources ADD COLUMN full_text TEXT;

-- +goose Down
ALTER TABLE sources DROP COLUMN full_text;
