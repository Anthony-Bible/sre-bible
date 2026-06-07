-- +goose Up
ALTER TABLE sources ADD COLUMN description TEXT;

-- +goose Down
ALTER TABLE sources DROP COLUMN description;
