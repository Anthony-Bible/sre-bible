-- +goose Up
CREATE TABLE contact_emails (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  session_id   UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  sender_name  TEXT NOT NULL,
  sender_email TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 1-per-session cap enforced at schema level (defense in depth)
CREATE UNIQUE INDEX contact_emails_one_per_session ON contact_emails (session_id);
CREATE INDEX contact_emails_created_idx ON contact_emails (created_at);

-- +goose Down
DROP TABLE IF EXISTS contact_emails;
