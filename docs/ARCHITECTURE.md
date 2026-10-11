# PeakChat — Architecture

Multi-tenant RAG chatbot platform. Customers sign up, create bots, upload
knowledge, write instructions, customise a widget, and embed it on their site
with one `<script>` tag.

The platform lives in two repositories:

| Repository     | Contains                                   |
| -------------- | ------------------------------------------ |
| `Rag-Backend`  | `api/` (Go), `rag/` (Python), compose, docs |
| `Rag-Frontend` | `web/` (Next.js dashboard + widget iframe), `widget/` (loader) |

Clone them side by side (`./Rag-Backend`, `./Rag-Frontend`); the compose file
in `Rag-Backend` builds the frontend from `../Rag-Frontend` when the
`frontend` profile is enabled.

## 1. Request flow

```
                         ┌─────────────────────── public ───────────────────────┐
 Host website            │                                                      │
 <script widget.js> ─────┼──► web (Next.js)  /widget.js, /widget/<key> (iframe) │
       │  config fetch   │        │ dashboard: /api/* rewrite (same origin)     │
       └─────────────────┼──► api (Go/Gin) ◄┘                                   │
         iframe chat SSE │        │                                             │
                         └────────┼─────────────────────────────────────────────┘
                                  │ internal only (bearer token, Cloud Run ingress=internal)
                                  ▼
                              rag (FastAPI) ──► LLM / embedding providers
                                  │
              ┌───────────────────┴──────────────┐
              ▼                                  ▼
     PostgreSQL + pgvector                Object storage (GCS / local dir in dev)
     (schema owned by api)
```

* **api** owns the schema, auth, tenancy and every public endpoint. It is the
  only service the internet can reach (besides the static web app).
* **rag** is internal. It reads/writes `documents` (status columns) and
  `document_chunks`, and bumps `bots.knowledge_version`; it never runs DDL.
* Document ingestion is a **database-backed queue**: `documents.status =
  'pending'` rows are claimed with `FOR UPDATE SKIP LOCKED`. The API pokes the
  rag service after an upload for immediate pickup; a periodic poll and a
  lease timeout (`locked_at`) recover from crashes. No extra queue
  infrastructure is needed, and it is safe across many instances.

## 2. Folder structure

```
Rag-Backend/
├── api/                              Go 1.24, Gin + GORM
│   ├── cmd/api/main.go               `api serve` | `api migrate`
│   ├── internal/
│   │   ├── config/                   env parsing + validation
│   │   ├── logger/                   slog JSON logger
│   │   ├── database/                 GORM (pgx) connection + pool
│   │   ├── migrate/                  embedded, versioned SQL migrations, advisory-locked
│   │   │   └── sql/0001_…sql
│   │   ├── models/                   GORM models (pgvector-go for embeddings)
│   │   ├── auth/                     argon2id, OTP, JWT, refresh-token rotation, service
│   │   ├── mail/                     Sender interface, SMTP + log senders, async queue, templates
│   │   ├── ratelimit/                Limiter interface, in-memory + Redis implementations
│   │   ├── storage/                  ObjectStore interface, GCS + local implementations
│   │   ├── ragclient/                HTTP client for the internal rag service (SSE aware)
│   │   ├── bots/                     bot/widget/document domain logic + validation
│   │   └── httpapi/                  router, middleware, handlers
│   ├── integration/                  upload → ingest → chat end-to-end test (build tag)
│   └── Dockerfile
├── rag/                              Python 3.12+, FastAPI (async)
│   ├── app/
│   │   ├── main.py                   app factory, lifespan (pool, worker), health
│   │   ├── config.py                 pydantic-settings
│   │   ├── db.py                     asyncpg pool + pgvector codec
│   │   ├── security.py               internal bearer-token auth
│   │   ├── tokens.py                 token counting
│   │   ├── cache.py                  LRU + optional Redis
│   │   ├── storage.py                GCS / local object reader
│   │   ├── ingestion/                extractors, safe URL fetcher, chunker, worker
│   │   ├── embeddings/               provider interface: openai-compatible, voyage, hash (dev)
│   │   ├── retrieval/                hybrid search (vector + FTS, RRF)
│   │   ├── llm/                      provider interface: anthropic, openai-compatible, fake (dev)
│   │   ├── prompt.py                 system prompt + history budget
│   │   └── routes/                   /internal/* endpoints
│   ├── tests/
│   └── Dockerfile
├── docker-compose.yml
├── .env.example
└── docs/ARCHITECTURE.md

Rag-Frontend/
├── web/                              Next.js (App Router, TS, Tailwind)
│   ├── app/
│   │   ├── (auth)/login, signup, verify, forgot-password, reset-password
│   │   ├── dashboard/                bot list, create
│   │   ├── dashboard/bots/[id]/      knowledge | instructions | widget | test tabs
│   │   ├── widget/[publicKey]/       chat UI rendered inside the iframe
│   │   ├── api/[...path]/route.ts    same-origin streaming proxy to the API
│   │   └── widget.js/route.ts        serves the loader with runtime config
│   ├── lib/                          api client, auth context, SSE reader, markdown
│   ├── proxy.ts                      security headers; CSP frame-ancestors for /widget/*
│   └── Dockerfile
└── widget/
    ├── src/loader.js                 vanilla JS loader (no framework)
    ├── build.mjs                     esbuild → web/lib/widget-loader.generated.ts
    └── test/
```

## 3. Database schema

One PostgreSQL database with the `vector` extension. Migrations are plain SQL
files embedded in the Go binary, applied in order under a Postgres advisory
lock (safe when several Cloud Run instances boot at once), and recorded in
`schema_migrations`. The vector dimension is injected from `EMBEDDING_DIM` at
migration time. Changing it later means a new migration plus a re-embed.

```
users ─┬─< email_otps
       ├─< refresh_tokens          (family_id groups a rotation chain)
       └─< bots ─┬── widget_configs (1:1)
                 ├─< documents ──< document_chunks
                 ├─< conversations ──< messages
                 └─< leads
```

| Table | Key columns | Notes |
| --- | --- | --- |
| `users` | `id uuid`, `email` (unique, lower-cased), `password_hash`, `email_verified`, timestamps | argon2id PHC string |
| `email_otps` | `user_id`, `code_hash`, `purpose` (`verify_email`/`login`/`reset_password`), `expires_at`, `attempts`, `consumed_at`, `created_at` | HMAC-SHA256 with a server pepper; index `(user_id, purpose, created_at desc)` |
| `refresh_tokens` | `user_id`, `family_id`, `token_hash` (unique), `expires_at`, `revoked_at`, `replaced_by`, `user_agent`, `ip` | SHA-256 of a 256-bit random token; reuse ⇒ revoke family |
| `bots` | `id`, `owner_id`, `name`, `public_key` (unique), `system_instructions`, `fallback_message`, `model`, `temperature`, `allowed_origins text[]`, `knowledge_version`, timestamps | `knowledge_version` keys the answer cache |
| `widget_configs` | `bot_id` (pk), `theme`, `content`, `behavior`, `features` (jsonb), `config_version` | version used as ETag |
| `documents` | `bot_id`, `filename`, `source_type`, `source_url`, `storage_key`, `mime_type`, `size_bytes`, `status`, `error`, `checksum`, `chunk_count`, `attempts`, `locked_at`, timestamps | partial index on active statuses for the worker |
| `document_chunks` | `id bigint`, `document_id`, `bot_id`, `chunk_index`, `content`, `token_count`, `embedding vector(N)`, `tsv` (generated), `metadata jsonb` | HNSW (`vector_cosine_ops`), GIN on `tsv`, B-tree on `bot_id` and `document_id` |
| `conversations` | `bot_id`, `visitor_id`, `channel` (`widget`/`test`), `visitor_meta jsonb`, `origin`, `created_at`, `last_message_at` | |
| `messages` | `conversation_id`, `bot_id`, `role`, `content`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `model`, `latency_ms`, `created_at` | usage logged per assistant message |
| `leads` | `bot_id`, `conversation_id`, `name`, `email`, `phone`, `created_at` | optional feature |

All foreign keys cascade on delete, so deleting a document removes its chunks
and deleting a bot removes everything under it.

`document_chunks.tsv` is a **generated column**
(`setweight(section title, 'A') || setweight(content, 'B')`). Postgres keeps it
in sync, and the rag service only inserts `content`, `metadata` and `embedding`.

## 4. Authentication

* Signup → argon2id hash → `verify_email` OTP mailed → `202 verification_required`.
  The same response comes back for an existing email, so the endpoint can't be
  used to find out which emails have accounts.
* Login with an unverified email: correct password ⇒ new OTP, `verification_required`, no tokens.
  Unknown email still runs one argon2id hash so the timing matches.
* OTP: 6 digits from `crypto/rand`, 10 min TTL, 5 attempts, 60 s resend
  cooldown, single use; issuing a code consumes any outstanding code of the
  same purpose.
* Tokens: HS256 JWT access token (15 min, held in memory by the dashboard) +
  opaque refresh token (30 days) in an `httpOnly; Secure; SameSite=Strict`
  cookie scoped to `/api/v1/auth`. Every refresh rotates. Presenting an already
  rotated token outside a 15-second race window revokes the whole family.
* The dashboard calls the API through a same-origin Next.js proxy route (`/api/*`),
  so the refresh cookie is first-party everywhere, including on `*.run.app`. A readable
  `pc_has_session=1` hint cookie (no secret) lets the dashboard skip the refresh call for
  visitors who have no session.
* Rate limits (per IP and per email) on every auth route. Bot/document/config
  queries always include `owner_id = <jwt sub>`; cross-tenant IDs return 404.

## 5. Widget security model

* `widget.js` runs on the host page and fetches `/v1/widget/<key>/config`.
  The browser sets `Origin`; the API rejects origins outside
  `allowed_origins`. An empty list means any origin, and the dashboard shows a
  warning when it is empty.
* The chat UI runs in an iframe served from the web origin. Next.js middleware
  sends `Content-Security-Policy: frame-ancestors 'self' <allowed_origins>`, so
  only allowed sites can frame it.
* Chat calls from the iframe have the web origin, which the API always
  accepts for public widget routes. That is safe because only allowed sites
  can embed the iframe.
* Per-IP and per-visitor rate limits on config, chat and leads.
* postMessage: the loader accepts messages only from the iframe's
  `contentWindow` and the web origin. The iframe accepts messages only from
  `window.parent` with an origin that passes the bot's allow-list.

## 6. RAG pipeline

**Ingest** (`rag` worker): claim document → download from storage (or fetch
the URL through an SSRF-safe fetcher) → extract text with structure (headings,
paragraphs) → checksum. If the checksum is unchanged and chunks already exist,
the document is marked ready without re-embedding → structure-aware chunks of
~500 tokens with ~50 tokens of overlap, each carrying its section title →
batched embeddings (cached by content hash) → insert chunks in one
transaction → `ready`, then bump `bots.knowledge_version`.

**Retrieve**: embed the query (LRU/Redis cached) → one SQL statement with two
CTEs, HNSW cosine top-N and `ts_rank_cd` top-N, both filtered by `bot_id` →
Reciprocal Rank Fusion (k = 60) → drop candidates below the similarity
threshold → top-k (default 5).

**Generate**: system prompt = static grounding rules + bot instructions,
marked with `cache_control` so the provider caches the prefix. Retrieved
context goes in the final user turn, so history stays small and the prefix
stays stable. History is the last N turns, trimmed to a token budget. When
nothing relevant is retrieved, the model is told to use the bot's fallback
message. Tokens stream rag → api → widget as SSE. The API stores the
assistant message and token usage when the `done` event arrives. First-turn
answers are cached under (bot, knowledge_version, config hash, normalised
question).
