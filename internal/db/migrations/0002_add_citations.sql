-- +goose Up
ALTER TABLE messages ADD COLUMN citations TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE messages DROP COLUMN citations;
