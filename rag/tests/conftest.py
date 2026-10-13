"""Shared fixtures.

Database tests need TEST_DATABASE_URL (a Postgres with pgvector where the user may
CREATE DATABASE). The schema is created from the Go API's migration files, which are the
single source of truth; this service never defines schema itself.
"""

from __future__ import annotations

import os
import secrets
import uuid
from pathlib import Path

import asyncpg
import pytest

from app.config import Settings
from app.db import create_pool

MIGRATIONS = Path(__file__).resolve().parents[2] / "api" / "internal" / "migrate" / "sql"
TEST_DIM = 384


def make_settings(**overrides) -> Settings:
    base = dict(
        app_env="test", database_url=os.environ.get("TEST_DATABASE_URL", "postgresql://unused/db"),
        rag_internal_token="t" * 32, embedding_provider="hash", embedding_dim=TEST_DIM, llm_provider="fake",
        tokenizer="heuristic", worker_concurrency=1, worker_poll_seconds=0.2, retrieval_min_similarity=0.15,
        chunk_min_tokens=20,
    )
    base.update(overrides)
    return Settings(**base)


@pytest.fixture(scope="session")
async def database_url():
    base = os.environ.get("TEST_DATABASE_URL")
    if not base:
        pytest.skip("TEST_DATABASE_URL not set")
    name = "hv_rag_test_" + secrets.token_hex(4)
    admin = await asyncpg.connect(base)
    await admin.execute(f"CREATE DATABASE {name}")
    await admin.close()
    url = base.rsplit("/", 1)[0] + "/" + name
    conn = await asyncpg.connect(url)
    for f in sorted(MIGRATIONS.glob("*.sql")):
        await conn.execute(f.read_text().replace("{{EMBEDDING_DIM}}", str(TEST_DIM)))
    await conn.close()
    yield url
    admin = await asyncpg.connect(base)
    await admin.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
    await admin.close()


@pytest.fixture(scope="session")
async def pool(database_url):
    p = await create_pool(database_url, 1, 5)
    yield p
    await p.close()


async def make_bot(pool) -> str:
    async with pool.acquire() as conn:
        uid = await conn.fetchval(
            "INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id",
            f"{uuid.uuid4().hex}@example.com",
        )
        bid = await conn.fetchval(
            "INSERT INTO bots (owner_id, name, public_key, model) VALUES ($1, 'b', $2, 'claude-haiku-5-5') RETURNING id",
            uid, "pk_" + secrets.token_urlsafe(18)[:24],
        )
    return str(bid)


async def make_document(pool, bot_id: str, source_type: str = "md", storage_key: str | None = None,
                        source_url: str | None = None, filename: str = "doc.md") -> str:
    async with pool.acquire() as conn:
        did = await conn.fetchval(
            """INSERT INTO documents (bot_id, filename, source_type, storage_key, source_url)
               VALUES ($1, $2, $3, $4, $5) RETURNING id""",
            bot_id, filename, source_type, storage_key, source_url,
        )
    return str(did)
