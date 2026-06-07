-- +goose Up
ALTER TABLE sources ADD COLUMN IF NOT EXISTS description TEXT;

-- +goose Down
ALTER TABLE sources DROP COLUMN description;
