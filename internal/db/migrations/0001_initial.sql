-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE sources (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  type        TEXT NOT NULL CHECK (type IN ('pdf','url')),
  location    TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chunks (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id   BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  idx         INT NOT NULL,
  content     TEXT NOT NULL,
  embedding   VECTOR(768) NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, idx)
);
CREATE INDEX chunks_embedding_idx ON chunks
  USING hnsw (embedding vector_cosine_ops);

CREATE TABLE sessions (
  id          UUID PRIMARY KEY,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE messages (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  role        TEXT NOT NULL CHECK (role IN ('user','assistant')),
  content     TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_session_idx ON messages (session_id, created_at);

-- +goose Down
DROP INDEX IF EXISTS messages_session_idx;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sessions;
DROP INDEX IF EXISTS chunks_embedding_idx;
DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS sources;
DROP EXTENSION IF EXISTS vector;
