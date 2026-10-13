"""FastAPI application factory for the internal rag service."""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from app.cache import TieredCache
from app.chat import ChatService
from app.config import Settings, get_settings
from app.db import create_pool, inspect_schema
from app.embeddings.providers import build_embedder, embedding_cache
from app.ingestion.chunker import Chunker
from app.ingestion.worker import IngestWorker
from app.llm.router import LLMRouter
from app.logging_setup import setup_logging
from app.retrieval.hybrid import HybridRetriever
from app.routes.internal import router as internal_router
from app.storage import GCSReader, LocalReader
from app.tokens import TokenCounter

log = logging.getLogger("rag")


def create_app(settings: Settings | None = None, start_worker: bool = True) -> FastAPI:
    settings = settings or get_settings()
    setup_logging(settings.log_level)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        pool = await create_pool(settings.database_url, settings.db_pool_min, settings.db_pool_max)
        schema = await inspect_schema(pool)
        if schema.embedding_dim != settings.embedding_dim:
            await pool.close()
            raise RuntimeError(
                f"EMBEDDING_DIM={settings.embedding_dim} but document_chunks.embedding is vector({schema.embedding_dim}); "
                "both services must use the same dimension"
            )
        redis = None
        if settings.redis_url:
            import redis.asyncio as aioredis

            redis = aioredis.from_url(settings.redis_url, socket_timeout=0.5, socket_connect_timeout=0.5)
        counter = TokenCounter(settings.tokenizer)
        embedder = build_embedder(settings, embedding_cache(settings, redis))
        storage = GCSReader(settings.gcs_bucket) if settings.storage_driver == "gcs" else LocalReader(settings.local_storage_dir)
        chunker = Chunker(counter, settings.chunk_target_tokens, settings.chunk_max_tokens,
                          settings.chunk_min_tokens, settings.chunk_overlap_tokens)
        llms = LLMRouter(settings)
        answers = TieredCache("ans", settings.answer_cache_size, settings.answer_cache_ttl_seconds, redis)
        retriever = HybridRetriever(pool, settings.fts_config, iterative_scan=schema.supports_iterative_scan)

        app.state.settings = settings
        app.state.log = log
        app.state.pool = pool
        app.state.chat = ChatService(settings, embedder, retriever, llms, answers, counter)
        app.state.worker = IngestWorker(settings, pool, storage, embedder, chunker)
        if start_worker:
            app.state.worker.start()
        log.info("rag service started", extra={
            "embedding_provider": settings.embedding_provider, "embedding_dim": settings.embedding_dim,
            "pgvector": ".".join(map(str, schema.pgvector_version)), "exact_tokenizer": counter.exact,
        })
        try:
            yield
        finally:
            log.info("shutting down")
            await app.state.worker.stop()
            await llms.aclose()
            await embedder.aclose()
            if redis is not None:
                await redis.aclose()
            await pool.close()

    app = FastAPI(
        title="Horizon Valley RAG (internal)",
        lifespan=lifespan,
        docs_url=None if settings.is_production else "/docs",
        redoc_url=None,
        openapi_url=None if settings.is_production else "/openapi.json",
    )
    app.state.settings = settings
    app.include_router(internal_router)

    @app.get("/healthz")
    async def healthz() -> dict:
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz(request: Request):
        try:
            async with request.app.state.pool.acquire() as conn:
                await conn.fetchval("SELECT 1")
        except Exception:
            return JSONResponse({"status": "unavailable"}, status_code=503)
        return {"status": "ok"}

    return app


def app_factory() -> FastAPI:  # uvicorn --factory app.main:app_factory
    return create_app()
