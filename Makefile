TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable

.PHONY: up down migrate test test-api test-rag e2e lint

up:            ## start the local stack
	docker compose up --build

down:
	docker compose down

migrate:       ## apply migrations using .env
	cd api && set -a && . ../.env && set +a && go run ./cmd/api migrate

test: test-api test-rag e2e

test-api:
	cd api && GIN_MODE=release TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./...

test-rag:
	cd rag && TEST_DATABASE_URL="$(subst ?sslmode=disable,,$(TEST_DATABASE_URL))" .venv/bin/pytest -q

e2e:
	ADMIN_DATABASE_URL="$(TEST_DATABASE_URL)" ./scripts/integration.sh

lint:
	cd api && gofmt -l . && go vet ./...
	cd rag && .venv/bin/ruff check .
