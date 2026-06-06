.PHONY: db-up db-down migrate test ingest query deps lint serve build-server

DATABASE_URL ?= postgres://sre:sre@localhost:5432/sre_bible?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)

# Detect Podman socket when Docker Desktop is not running
PODMAN_SOCK := $(shell podman machine inspect --format "{{.ConnectionInfo.PodmanSocket.Path}}" 2>/dev/null)
ifneq ($(PODMAN_SOCK),)
  export DOCKER_HOST = unix://$(PODMAN_SOCK)
endif

db-up:
	docker-compose up -d
	@echo "Waiting for Postgres to be healthy..."
	@until docker-compose exec -T postgres pg_isready -U sre -d sre_bible >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres ready."

db-down:
	docker-compose down

migrate: db-up
	DATABASE_URL=$(DATABASE_URL) go run ./cmd/ingest migrate

test:
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test ./... -v -count=1

test-unit:
	go test ./internal/ingest/... -v -count=1

test-integration: db-up
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test ./internal/db/... -v -count=1

ingest:
	@if [ -z "$(SRC)" ]; then echo "Usage: make ingest SRC=<path-or-url>"; exit 1; fi
	DATABASE_URL=$(DATABASE_URL) GEMINI_API_KEY=$(GEMINI_API_KEY) go run ./cmd/ingest "$(SRC)"

query:
	@if [ -z "$(Q)" ]; then echo "Usage: make query Q=\"<question>\""; exit 1; fi
	DATABASE_URL=$(DATABASE_URL) GEMINI_API_KEY=$(GEMINI_API_KEY) ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY) \
		go run ./cmd/query "$(Q)"

deps:
	go mod tidy
	go mod download

lint:
	go vet ./...

PORT ?= 8080

serve: db-up
	DATABASE_URL=$(DATABASE_URL) GEMINI_API_KEY=$(GEMINI_API_KEY) \
	ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY) LISTEN_ADDR=:$(PORT) \
	go run ./cmd/server

build-server:
	go build -o bin/server ./cmd/server
