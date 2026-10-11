"""Background ingestion worker backed by the documents table.

Claiming uses FOR UPDATE SKIP LOCKED, so any number of workers across any number of
instances cooperate safely. A crashed worker's lease (locked_at) expires and the document
is retried, up to WORKER_MAX_ATTEMPTS. The Go API pokes /internal/ingest/{id} after an
upload for immediate pickup; a periodic poll catches anything missed.
"""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import json
import logging
import time
from dataclasses import dataclass

import asyncpg

from app.config import Settings
from app.embeddings.providers import CachedEmbedder, EmbeddingError
from app.ingestion.chunker import Chunk, Chunker
from app.ingestion.extractors import ExtractionError, extract
from app.ingestion.fetch import FetchError, fetch_url
from app.storage import ObjectReader, StorageError

log = logging.getLogger(__name__)

CLAIM_SQL = """
UPDATE documents d
SET status = 'processing', locked_at = now(), attempts = d.attempts + 1, updated_at = now()
WHERE d.id = (
    SELECT id FROM documents
    WHERE (status = 'pending' AND (locked_at IS NULL OR locked_at < now() - make_interval(secs => $2)))
       OR (status = 'processing' AND locked_at < now() - make_interval(secs => $1))
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING d.id, d.bot_id, d.filename, d.source_type, d.source_url, d.storage_key, d.checksum,
          d.chunk_count, d.attempts
"""

# Same as CLAIM_SQL but for one specific document (the API's "ingest now" poke).
CLAIM_ONE_SQL = """
UPDATE documents d
SET status = 'processing', locked_at = now(), attempts = d.attempts + 1, updated_at = now()
WHERE d.id = (
    SELECT id FROM documents
    WHERE id = $3
      AND ((status = 'pending' AND (locked_at IS NULL OR locked_at < now() - make_interval(secs => $2)))
           OR (status = 'processing' AND locked_at < now() - make_interval(secs => $1)))
    FOR UPDATE SKIP LOCKED
)
RETURNING d.id, d.bot_id, d.filename, d.source_type, d.source_url, d.storage_key, d.checksum,
          d.chunk_count, d.attempts
"""


class PermanentError(Exception):
    """Retrying will not help (bad file, unsupported content, …)."""


@dataclass
class Job:
    id: str
    bot_id: str
    filename: str
    source_type: str
    source_url: str | None
    storage_key: str | None
    checksum: str | None
    chunk_count: int
    attempts: int


class IngestWorker:
    def __init__(self, settings: Settings, pool: asyncpg.Pool, storage: ObjectReader, embedder: CachedEmbedder, chunker: Chunker):
        self.s = settings
        self.pool = pool
        self.storage = storage
        self.embedder = embedder
        self.chunker = chunker
        self._wake = asyncio.Event()
        self._stop = asyncio.Event()
        self._tasks: list[asyncio.Task] = []

    # ---- lifecycle ----------------------------------------------------------

    def start(self) -> None:
        for i in range(self.s.worker_concurrency):
            self._tasks.append(asyncio.create_task(self._loop(i), name=f"ingest-worker-{i}"))

    def wake(self) -> None:
        self._wake.set()

    async def stop(self, timeout: float = 20.0) -> None:
        self._stop.set()
        self._wake.set()
        if not self._tasks:
            return
        _, pending = await asyncio.wait(self._tasks, timeout=timeout)
        for t in pending:  # documents mid-flight are re-claimed after the lease expires
            t.cancel()
        with contextlib.suppress(Exception):
            await asyncio.gather(*pending, return_exceptions=True)

    async def _loop(self, n: int) -> None:
        while not self._stop.is_set():
            try:
                job = await self.claim()
            except Exception:
                log.exception("claim failed")
                job = None
            if job is not None:
                await self.process(job)
                continue
            self._wake.clear()
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._wake.wait(), timeout=self.s.worker_poll_seconds)

    # ---- claiming -------------------------------------------------------------

    async def claim(self, document_id: str | None = None) -> Job | None:
        retry_backoff = 30  # seconds before a transiently failed document is retried
        async with self.pool.acquire() as conn:
            if document_id:
                row = await conn.fetchrow(CLAIM_ONE_SQL, self.s.worker_lease_seconds, retry_backoff, document_id)
            else:
                row = await conn.fetchrow(CLAIM_SQL, self.s.worker_lease_seconds, retry_backoff)
        if row is None:
            return None
        return Job(id=str(row["id"]), bot_id=str(row["bot_id"]), filename=row["filename"], source_type=row["source_type"],
                   source_url=row["source_url"], storage_key=row["storage_key"], checksum=row["checksum"],
                   chunk_count=row["chunk_count"], attempts=row["attempts"])

    # ---- processing -----------------------------------------------------------

    async def process(self, job: Job) -> None:
        started = time.monotonic()
        extra = {"document_id": job.id, "bot_id": job.bot_id, "source_type": job.source_type, "attempt": job.attempts}
        if job.attempts > self.s.worker_max_attempts:
            await self._fail(job, "Processing failed repeatedly. Please re-upload the document.")
            log.error("document exceeded max attempts", extra=extra)
            return
        try:
            result = await self._ingest(job)
            log.info("document ingested", extra={**extra, **result, "ms": int((time.monotonic() - started) * 1000)})
        except (PermanentError, ExtractionError, StorageError) as exc:
            log.warning("document failed", extra={**extra, "error": str(exc)})
            await self._fail(job, str(exc))
        except FetchError as exc:
            log.warning("url fetch failed", extra={**extra, "error": str(exc), "permanent": exc.permanent})
            if exc.permanent or job.attempts >= self.s.worker_max_attempts:
                await self._fail(job, str(exc))
            else:
                await self._retry_later(job)
        except (EmbeddingError, asyncpg.PostgresError, OSError) as exc:
            log.error("document ingestion error (will retry)", extra={**extra, "error": str(exc)})
            if job.attempts >= self.s.worker_max_attempts:
                await self._fail(job, "Temporary processing error. Please try again later.")
            else:
                await self._retry_later(job)
        except Exception:
            log.exception("unexpected ingestion error", extra=extra)
            await self._fail(job, "Unexpected processing error.")

    async def _load(self, job: Job) -> tuple[str, bytes]:
        """Returns (effective source type for extraction, raw bytes)."""
        if job.source_type == "url":
            if not job.source_url:
                raise PermanentError("Document has no URL")
            fetched = await fetch_url(job.source_url, self.s.max_fetch_bytes, self.s.fetch_timeout_seconds, self.s.fetch_trust_env)
            kind = {"application/pdf": "pdf", "text/plain": "txt", "text/markdown": "md"}.get(fetched.content_type, "html")
            return kind, fetched.data
        if not job.storage_key:
            raise PermanentError("Document has no stored file")
        return job.source_type, await self.storage.read(job.storage_key, self.s.max_fetch_bytes * 5)

    async def _ingest(self, job: Job) -> dict:
        kind, data = await self._load(job)
        checksum = hashlib.sha256(data).hexdigest()

        async with self.pool.acquire() as conn:
            existing = await conn.fetch("SELECT id, metadata, embedding FROM document_chunks WHERE document_id = $1", job.id)
        # Unchanged source that is already indexed: nothing to do (e.g. URL re-sync).
        if job.checksum == checksum and job.chunk_count > 0 and existing:
            await self._mark_ready(job, checksum, len(existing), bump_version=False)
            return {"skipped": "unchanged", "chunks": len(existing)}

        title, blocks = await asyncio.to_thread(extract, kind, data, self.s.max_extract_chars)
        if job.source_type == "url" and not title:
            title = job.filename
        chunks: list[Chunk] = self.chunker.chunk(blocks, doc_title=title if job.source_type == "url" else "")
        if not chunks:
            raise PermanentError("No text could be extracted. Scanned PDFs need OCR before upload.")

        # Reuse embeddings of chunks whose text did not change (cheap re-ingestion).
        reuse: dict[str, list[float]] = {}
        for r in existing:
            meta = r["metadata"] if isinstance(r["metadata"], dict) else json.loads(r["metadata"])
            if meta.get("hash") and meta.get("embedding_model") == self.embedder.model:
                reuse[meta["hash"]] = r["embedding"]
        todo = [c for c in chunks if c.content_hash not in reuse]
        fresh = await self.embedder.embed([c.embed_text for c in todo], "document") if todo else []
        vectors = dict(reuse)
        for c, v in zip(todo, fresh, strict=True):
            vectors[c.content_hash] = v

        records = []
        for c in chunks:
            meta = {"section": c.section, "heading_path": c.heading_path, "hash": c.content_hash,
                    "embedding_model": self.embedder.model, "source": job.filename}
            if c.page is not None:
                meta["page"] = c.page
            records.append((job.id, job.bot_id, c.index, c.content, c.token_count, vectors[c.content_hash], json.dumps(meta)))

        async with self.pool.acquire() as conn, conn.transaction():
            # Re-check the claim: the document may have been deleted or re-queued meanwhile.
            still = await conn.fetchval("SELECT 1 FROM documents WHERE id = $1 AND status = 'processing' FOR UPDATE", job.id)
            if not still:
                return {"skipped": "gone"}
            await conn.execute("DELETE FROM document_chunks WHERE document_id = $1", job.id)
            await conn.executemany(
                """INSERT INTO document_chunks (document_id, bot_id, chunk_index, content, token_count, embedding, metadata)
                   VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)""",
                records,
            )
            await self._mark_ready_conn(conn, job, checksum, len(chunks), bump_version=True)
        return {"chunks": len(chunks), "embedded": len(todo), "reused": len(chunks) - len(todo),
                "tokens": sum(c.token_count for c in chunks)}

    async def _mark_ready(self, job: Job, checksum: str, n: int, bump_version: bool) -> None:
        async with self.pool.acquire() as conn, conn.transaction():
            await self._mark_ready_conn(conn, job, checksum, n, bump_version)

    @staticmethod
    async def _mark_ready_conn(conn: asyncpg.Connection, job: Job, checksum: str, n: int, bump_version: bool) -> None:
        await conn.execute(
            """UPDATE documents SET status = 'ready', error = NULL, checksum = $2, chunk_count = $3,
                   locked_at = NULL, updated_at = now()
               WHERE id = $1""",
            job.id, checksum, n,
        )
        if bump_version:  # invalidates cached answers for this bot
            await conn.execute("UPDATE bots SET knowledge_version = knowledge_version + 1 WHERE id = $1", job.bot_id)

    async def _fail(self, job: Job, message: str) -> None:
        async with self.pool.acquire() as conn:
            await conn.execute(
                "UPDATE documents SET status = 'failed', error = $2, locked_at = NULL, updated_at = now() WHERE id = $1",
                job.id, message[:500],
            )

    async def _retry_later(self, job: Job) -> None:
        # Back to pending; locked_at doubles as "not before" for the retry backoff in CLAIM_SQL.
        async with self.pool.acquire() as conn:
            await conn.execute(
                "UPDATE documents SET status = 'pending', locked_at = now(), updated_at = now() WHERE id = $1", job.id
            )
