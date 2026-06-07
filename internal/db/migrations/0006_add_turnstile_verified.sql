-- +goose Up
ALTER TABLE sessions ADD COLUMN turnstile_verified BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN turnstile_verified;
