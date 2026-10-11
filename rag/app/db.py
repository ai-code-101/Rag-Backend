"""Pooled asyncpg connections with the pgvector codec registered.

The schema is owned by the Go API. This service only reads/writes rows and verifies at
startup that the vector dimension matches its embedding configuration.
"""

from __future__ import annotations

import logging

import asyncpg
from pgvector.asyncpg import register_vector

log = logging.getLogger(__name__)


async def _init(conn: asyncpg.Connection) -> None:
    await register_vector(conn)


async def create_pool(dsn: str, min_size: int, max_size: int) -> asyncpg.Pool:
    return await asyncpg.create_pool(
        dsn,
        min_size=min_size,
        max_size=max_size,
        init=_init,
        command_timeout=60,
        max_inactive_connection_lifetime=300,
        # Statement cache works with direct connections; set PGBOUNCER-style poolers to
        # session mode or disable caching if a transaction pooler sits in between.
        statement_cache_size=256,
    )


class SchemaInfo:
    def __init__(self, embedding_dim: int, pgvector_version: tuple[int, ...]):
        self.embedding_dim = embedding_dim
        self.pgvector_version = pgvector_version

    @property
    def supports_iterative_scan(self) -> bool:
        return self.pgvector_version >= (0, 8, 0)


async def inspect_schema(pool: asyncpg.Pool) -> SchemaInfo:
    async with pool.acquire() as conn:
        dim = await conn.fetchval(
            """SELECT atttypmod FROM pg_attribute
               WHERE attrelid = 'document_chunks'::regclass AND attname = 'embedding'"""
        )
        ver = await conn.fetchval("SELECT extversion FROM pg_extension WHERE extname = 'vector'")
    parts = tuple(int(p) for p in str(ver).split(".") if p.isdigit())
    return SchemaInfo(int(dim), parts)
