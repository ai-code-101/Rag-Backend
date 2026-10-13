# Horizon Valley — multi-tenant RAG chatbot platform (backend)

Customers sign up, create bots, upload knowledge (PDF, DOCX, TXT, Markdown, URLs, pasted
text), write custom instructions, style a widget and embed it on their site with one
`<script>` tag. Visitors chat with an assistant that answers **only** from that knowledge.

This repository holds the backend services. The dashboard and the widget live in
[`Rag-Frontend`](https://github.com/ai-code-101/Rag-Frontend).

| Path | Service | Stack |
| --- | --- | --- |
| `api/` | Public API: auth, bots, documents, widget config, chat gateway. Owns the schema. | Go, Gin, GORM, pgvector-go |
| `rag/` | Internal service: ingestion, chunking, embeddings, hybrid retrieval, LLM streaming. | Python, FastAPI, asyncpg |
| `docs/ARCHITECTURE.md` | Folder structure, schema, request flows, security model. | |

## Quick start (Docker)

```bash
git clone https://github.com/ai-code-101/Rag-Backend.git
git clone https://github.com/ai-code-101/Rag-Frontend.git   # side by side
cd Rag-Backend
cp .env.example .env
# Fill in JWT_SECRET and RAG_INTERNAL_TOKEN (openssl rand -base64 48), then either
#  • OpenRouter: OPENROUTER_API_KEY (chat + embeddings), or
#  • fully offline: EMBEDDING_PROVIDER=hash LLM_PROVIDER=fake
docker compose --profile frontend up --build
```

* Dashboard: http://localhost:3000 (sign up and you're signed in straight away)
* API: http://localhost:8080 (`/healthz`, `/readyz`)
* The rag service is reachable only on the compose network, never from the host.

## Local development without Docker

Prerequisites: Go 1.26, Python 3.12+, PostgreSQL 16 with pgvector ≥ 0.5 (0.8+ recommended).

```bash
# database
createdb horizonvalley && psql horizonvalley -c 'CREATE EXTENSION IF NOT EXISTS vector'

# api
cd api
set -a; . ../.env; set +a
go run ./cmd/api migrate     # or rely on MIGRATE_ON_START=true
go run ./cmd/api             # :8080

# rag
cd ../rag
python -m venv .venv && .venv/bin/pip install -r requirements.txt -r requirements-dev.txt
set -a; . ../.env; set +a
DATABASE_URL=postgresql://horizonvalley:horizonvalley@localhost:5432/horizonvalley \
  .venv/bin/uvicorn --factory app.main:app_factory --port 8000 --reload
```

## Migrations

The Go service owns the schema. Migrations are plain SQL files in
`api/internal/migrate/sql/`, embedded into the binary and applied in order:

* `api migrate` applies them and exits (use this as a Cloud Run job or a release step);
* `api serve` applies them on start when `MIGRATE_ON_START=true` (the default).

A Postgres advisory lock means concurrently starting instances never race, and every
migration runs in a transaction together with its `schema_migrations` row. GORM has no
type for `vector(N)`, generated `tsvector` columns or HNSW indexes, so the
`document_chunks` table is defined in SQL (`0003_document_chunks.sql`) and only *mapped*
by GORM (via `pgvector-go`). The rag service never runs DDL. At startup it checks
that `document_chunks.embedding` has the dimension it is configured for, and refuses to
start if they differ.

To add a migration, create the next numbered file (e.g. `0005_add_x.sql`). Never edit
an applied one. `{{EMBEDDING_DIM}}` is substituted from `EMBEDDING_DIM`.

## Tests

```bash
make test-api   # Go unit + DB tests (auth, refresh rotation, tenancy, widget, uploads, SSE)
make test-rag   # Python unit + DB tests (chunking, extraction, SSRF, retrieval, worker, API, OpenRouter)
make e2e        # upload → ingest → chat across both services (no external APIs)
```

DB tests need `TEST_DATABASE_URL` pointing to a Postgres with pgvector where the user
can `CREATE DATABASE`; each run gets a throwaway database, and the tests are skipped
when it is unset. The rag tests build their schema from the Go migration files, so
both services are always tested against the same schema. CI runs all three suites
(`.github/workflows/ci.yml`).

## Environment variables

Everything is configured through the environment. `.env.example` documents every
variable. The most important ones:

| Variable | Service | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | both | Postgres DSN (`postgres://…` for Go, `postgresql://…` for asyncpg) |
| `EMBEDDING_DIM` | both | vector dimension; must match the embedding model and both services |
| `RAG_INTERNAL_TOKEN` | both | bearer token the API uses to call the rag service |
| `STORAGE_DRIVER`, `GCS_BUCKET` | both | `gcs` in production, `local` only for development |
| `JWT_SECRET` | api | ≥ 32 random bytes |
| `WEB_ORIGIN` | api | public origin of the web app (dashboard + widget iframe) |
| `COOKIE_SECURE`, `COOKIE_SAMESITE`, `COOKIE_PATH` | api | refresh-cookie attributes |
| `ALLOWED_MODELS`, `DEFAULT_MODEL` | api | models a bot may use, as `<provider>:<model>` (see below) |
| `REDIS_URL` | both | optional; shared rate limits (api) and caches (rag) across instances |
| `OPENROUTER_API_KEY` | rag | OpenRouter (chat and embeddings) |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` | rag | direct Anthropic / OpenAI access (optional, for later) |
| `EMBEDDING_PROVIDER`, `EMBEDDING_MODEL`, `VOYAGE_API_KEY` | rag | embeddings |
| `RETRIEVAL_TOP_K`, `RETRIEVAL_MIN_SIMILARITY` | rag | retrieval tuning |

### Models and providers

A bot's model id says which provider serves it:

| Model id | Provider | Key |
| --- | --- | --- |
| `openrouter:<vendor>/<model>`, e.g. `openrouter:openai/gpt-4o-mini` | [OpenRouter](https://openrouter.ai) | `OPENROUTER_API_KEY` |
| `claude-*`, e.g. `claude-haiku-5-5` | Anthropic API | `ANTHROPIC_API_KEY` |
| `openai:<model>`, e.g. `openai:gpt-4.1-mini` | OpenAI, or any OpenAI-compatible `OPENAI_BASE_URL` | `OPENAI_API_KEY` |

List the ones bots may pick in `ALLOWED_MODELS` (comma separated) and set `DEFAULT_MODEL`.
To add Anthropic or OpenAI later, set the key and append the model ids; no code changes
are needed. Embeddings are configured separately (`EMBEDDING_PROVIDER=openrouter` with
`EMBEDDING_MODEL=openai/text-embedding-3-small` and `EMBEDDING_DIM=1536` by default).

Accounts are created with email and password and are usable immediately. There is no
email verification or password-reset email.

## API overview

Dashboard (Bearer access token; refresh token in an `httpOnly` cookie):

```
POST /v1/auth/signup | login | refresh | logout
GET  /v1/me, /v1/models
GET|POST /v1/bots          GET|PATCH|DELETE /v1/bots/:id      POST /v1/bots/:id/rotate-key
GET|PUT  /v1/bots/:id/widget
GET  /v1/bots/:id/documents     POST /v1/bots/:id/documents (multipart) | documents/url | documents/text
DELETE /v1/bots/:id/documents/:docId     POST /v1/bots/:id/documents/:docId/reingest
POST /v1/bots/:id/chat (SSE test chat)   GET /v1/bots/:id/usage | leads
```

Public widget (public key + origin allow-list + per-IP/visitor rate limits):

```
GET  /v1/widget/:key/config           (ETag, 60 s cache)
POST /v1/widget/:key/chat             (SSE: meta → sources → token… → done)
GET  /v1/widget/:key/conversations/:id?visitor_id=…
POST /v1/widget/:key/leads
```

## Deploying to Google Cloud Run

All three services are stateless containers. Uploaded files go to GCS; state lives in
Postgres (and optionally Memorystore Redis).

```bash
PROJECT=my-project REGION=europe-west1 REPO=horizonvalley
gcloud artifacts repositories create $REPO --repository-format=docker --location=$REGION

# 1. Database: Cloud SQL for PostgreSQL 16 (pgvector is available as an extension).
#    Create database "horizonvalley" and a user; the first migration runs CREATE EXTENSION vector.
# 2. Bucket for uploads (uniform access, no public access; add a lifecycle rule if desired).
gcloud storage buckets create gs://$PROJECT-horizon-valley-uploads --location=$REGION --uniform-bucket-level-access
# 3. Secrets: JWT_SECRET, RAG_INTERNAL_TOKEN, OPENROUTER_API_KEY, DATABASE_URL(s)
printf '%s' "$(openssl rand -base64 48)" | gcloud secrets create JWT_SECRET --data-file=-

# 4. Build and push
gcloud builds submit api --tag $REGION-docker.pkg.dev/$PROJECT/$REPO/api
gcloud builds submit rag --tag $REGION-docker.pkg.dev/$PROJECT/$REPO/rag

# 5. rag: internal ingress only; CPU always allocated so the ingestion worker keeps running.
gcloud run deploy horizon-valley-rag --image $REGION-docker.pkg.dev/$PROJECT/$REPO/rag --region $REGION \
  --ingress internal --no-allow-unauthenticated --no-cpu-throttling --min-instances 1 \
  --set-env-vars APP_ENV=production,STORAGE_DRIVER=gcs,GCS_BUCKET=$PROJECT-horizon-valley-uploads,EMBEDDING_DIM=1536,EMBEDDING_PROVIDER=openrouter,EMBEDDING_MODEL=openai/text-embedding-3-small \
  --set-secrets DATABASE_URL=RAG_DATABASE_URL:latest,RAG_INTERNAL_TOKEN=RAG_INTERNAL_TOKEN:latest,OPENROUTER_API_KEY=OPENROUTER_API_KEY:latest \
  --add-cloudsql-instances $PROJECT:$REGION:horizon-valley-db

# 6. Migrations as a job (or keep MIGRATE_ON_START=true)
gcloud run jobs deploy horizon-valley-migrate --image $REGION-docker.pkg.dev/$PROJECT/$REPO/api --args migrate ... && \
  gcloud run jobs execute horizon-valley-migrate --wait

# 7. api: public; egress through the VPC so it can reach the internal rag service.
gcloud run deploy horizon-valley-api --image $REGION-docker.pkg.dev/$PROJECT/$REPO/api --region $REGION \
  --allow-unauthenticated --network default --subnet default --vpc-egress all-traffic \
  --set-env-vars APP_ENV=production,STORAGE_DRIVER=gcs,GCS_BUCKET=$PROJECT-horizon-valley-uploads,WEB_ORIGIN=https://app.example.com,RAG_URL=https://horizon-valley-rag-xxxx.run.app,RAG_ID_TOKEN_AUDIENCE=https://horizon-valley-rag-xxxx.run.app,COOKIE_SECURE=true,MIGRATE_ON_START=false \
  --set-secrets DATABASE_URL=API_DATABASE_URL:latest,JWT_SECRET=JWT_SECRET:latest,RAG_INTERNAL_TOKEN=RAG_INTERNAL_TOKEN:latest \
  --add-cloudsql-instances $PROJECT:$REGION:horizon-valley-db
```

Notes:

* **Service accounts:** the api needs `roles/storage.objectAdmin` on the bucket; rag
  needs `roles/storage.objectViewer`. Both need `roles/cloudsql.client` and
  `roles/secretmanager.secretAccessor`. rag is protected by internal-only ingress *and* the
  shared bearer token. With `--no-allow-unauthenticated` it also requires Cloud Run IAM: set
  `RAG_ID_TOKEN_AUDIENCE=<rag URL>` on the api (it then sends a Google ID token in
  `X-Serverless-Authorization`) and grant the api's service account `roles/run.invoker` on rag.
* **Client IPs:** set `TRUSTED_PROXIES` to the ranges of the load balancer in front of the
  api so rate limits use the real visitor IP.
* **Multiple instances:** set `REDIS_URL` (Memorystore) so rate limits and caches are
  shared. Without it each instance enforces its own limits.
* **Connection pools:** keep `DB_MAX_OPEN_CONNS × api instances + DB_POOL_MAX × rag instances`
  below the Cloud SQL connection limit.
* **Graceful shutdown:** the api drains in-flight HTTP requests on SIGTERM (25 s). rag stops
  its workers, and any document still in flight is picked up again after its lease expires.

## Efficiency choices

* **Latency:** a single SQL round trip does vector + full-text retrieval and RRF; query
  embeddings and first-turn answers are cached; tokens stream end to end. If the visitor
  disconnects, the model stream is cancelled.
* **Tokens:** the system prompt is static first (rules → bot instructions) and marked for
  provider prompt caching. Retrieved context only ever goes in the latest user turn. History
  is the last *N* turns trimmed to a token budget, chunks below the similarity threshold
  are dropped, and Claude runs at `effort: low`. Usage is recorded per message (`messages.*_tokens`).
* **Infrastructure:** no separate queue: ingestion is a `SKIP LOCKED` job table. Unchanged
  documents are skipped by checksum, unchanged chunks reuse their stored embeddings,
  embeddings are batched, and `text-embedding-3-*` dimensions are configurable to shrink the index.
