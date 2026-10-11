#!/usr/bin/env bash
# End-to-end test: upload → ingest → chat across the Go API and the Python rag service.
#
# Needs a Postgres with pgvector reachable at ADMIN_DATABASE_URL (default: the
# docker-compose database on localhost:5432), Go and Python 3.12+. No external APIs are
# called: the rag service runs with hash embeddings and the fake LLM.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ADMIN_DATABASE_URL="${ADMIN_DATABASE_URL:-postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable}"
DB_NAME="pc_e2e_$RANDOM$RANDOM"
BASE="${ADMIN_DATABASE_URL%%\?*}"
DATABASE_URL="${BASE%/*}/${DB_NAME}?sslmode=disable"
RAG_PORT="${RAG_PORT:-18000}"
TOKEN="integration-internal-token-0123456789"
STORAGE_DIR="$(mktemp -d)"
DIM=256

cleanup() {
  [[ -n "${RAG_PID:-}" ]] && kill "$RAG_PID" 2>/dev/null || true
  psql "$ADMIN_DATABASE_URL" -qc "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$STORAGE_DIR"
}
trap cleanup EXIT

psql "$ADMIN_DATABASE_URL" -qc "CREATE DATABASE $DB_NAME"

echo "→ migrating"
(cd "$ROOT/api" && DATABASE_URL="$DATABASE_URL" EMBEDDING_DIM=$DIM JWT_SECRET="$(printf 'x%.0s' {1..40})" \
  OTP_PEPPER="$(printf 'y%.0s' {1..40})" RAG_INTERNAL_TOKEN="$TOKEN" MAIL_DRIVER=log go run ./cmd/api migrate)

echo "→ starting rag on :$RAG_PORT"
PY="${PYTHON:-$ROOT/rag/.venv/bin/python}"
[[ -x "$PY" ]] || PY=python3
(cd "$ROOT/rag" && APP_ENV=test DATABASE_URL="${DATABASE_URL%%\?*}" RAG_INTERNAL_TOKEN="$TOKEN" EMBEDDING_PROVIDER=hash \
  EMBEDDING_DIM=$DIM LLM_PROVIDER=fake TOKENIZER=heuristic LOCAL_STORAGE_DIR="$STORAGE_DIR" WORKER_POLL_SECONDS=0.5 \
  RETRIEVAL_MIN_SIMILARITY=0.15 CHUNK_MIN_TOKENS=20 LOG_LEVEL=warning \
  "$PY" -m uvicorn --factory app.main:app_factory --port "$RAG_PORT") &
RAG_PID=$!
for _ in $(seq 1 50); do curl -fs "http://localhost:$RAG_PORT/healthz" >/dev/null && break; sleep 0.2; done

echo "→ running e2e test"
cd "$ROOT/api"
INTEGRATION_DATABASE_URL="$DATABASE_URL" INTEGRATION_RAG_URL="http://localhost:$RAG_PORT" INTEGRATION_RAG_TOKEN="$TOKEN" \
  INTEGRATION_STORAGE_DIR="$STORAGE_DIR" GIN_MODE=release go test -tags integration -count=1 -v ./integration/...
