"""Ingestion worker and HTTP API against a real database (fake LLM, hash embeddings)."""

import asyncio
import json

import httpx
import pytest

from app.embeddings.providers import CachedEmbedder, HashEmbedder, embedding_cache
from app.ingestion.chunker import Chunker
from app.ingestion.worker import IngestWorker
from app.main import create_app
from app.storage import LocalReader
from app.tokens import TokenCounter
from tests.conftest import TEST_DIM, make_bot, make_document, make_settings

DOC = """# Acme Help Center

## Opening hours

Our support team is available Monday to Friday from 9am to 6pm CET. On weekends we answer emails only.

## Refund policy

You can request a full refund within 30 days of purchase. Refunds are paid back to the original payment method within 5 business days.
"""


class CountingEmbedder(HashEmbedder):
    def __init__(self):
        super().__init__(TEST_DIM)
        self.embedded = 0

    async def embed(self, texts, input_type):
        self.embedded += len(texts)
        return await super().embed(texts, input_type)


@pytest.fixture
def storage_dir(tmp_path):
    return tmp_path


def make_worker(pool, storage_dir, inner=None):
    s = make_settings(local_storage_dir=str(storage_dir))
    inner = inner or CountingEmbedder()
    # Fresh cache per worker so we can observe embedding reuse from the database.
    emb = CachedEmbedder(inner, embedding_cache(s))
    chunker = Chunker(TokenCounter("heuristic"), min_tokens=s.chunk_min_tokens)
    return IngestWorker(s, pool, LocalReader(str(storage_dir)), emb, chunker), inner


async def doc_row(pool, doc_id):
    async with pool.acquire() as conn:
        return await conn.fetchrow("SELECT status, error, chunk_count, checksum FROM documents WHERE id = $1", doc_id)


async def test_ingest_reingest_and_failure(pool, storage_dir):
    bot = await make_bot(pool)
    key = f"bots/{bot}/doc/help.md"
    (storage_dir / key).parent.mkdir(parents=True)
    (storage_dir / key).write_text(DOC)
    doc = await make_document(pool, bot, "md", storage_key=key, filename="help.md")

    worker, inner = make_worker(pool, storage_dir)
    job = await worker.claim(doc)
    assert job and job.attempts == 1
    assert await worker.claim(doc) is None  # claimed rows are skipped by other workers
    await worker.process(job)
    row = await doc_row(pool, doc)
    assert row["status"] == "ready" and row["chunk_count"] >= 1 and row["error"] is None
    async with pool.acquire() as conn:
        chunks = await conn.fetch("SELECT content, metadata FROM document_chunks WHERE document_id = $1 ORDER BY chunk_index", doc)
        kv = await conn.fetchval("SELECT knowledge_version FROM bots WHERE id = $1", bot)
    assert kv == 1
    meta = json.loads(chunks[0]["metadata"])
    assert meta["section"].startswith("Acme Help Center") and meta["hash"]
    first_embedded = inner.embedded

    # Unchanged source: re-queue → skipped without embedding anything.
    async with pool.acquire() as conn:
        await conn.execute("UPDATE documents SET status='pending', locked_at=NULL WHERE id=$1", doc)
    worker2, inner2 = make_worker(pool, storage_dir)
    await worker2.process(await worker2.claim(doc))
    assert inner2.embedded == 0 and (await doc_row(pool, doc))["status"] == "ready"

    # Edited source: only changed chunks are re-embedded (others reuse stored vectors).
    (storage_dir / key).write_text(DOC + "\n## Shipping\n\nWe ship to all EU countries within 3 days.\n")
    async with pool.acquire() as conn:
        await conn.execute("UPDATE documents SET status='pending', locked_at=NULL WHERE id=$1", doc)
    worker3, inner3 = make_worker(pool, storage_dir)
    await worker3.process(await worker3.claim(doc))
    assert 0 < inner3.embedded <= first_embedded + 1
    assert (await doc_row(pool, doc))["status"] == "ready"

    # Missing file → permanent failure with a user-facing message.
    bad = await make_document(pool, bot, "pdf", storage_key=f"bots/{bot}/missing.pdf")
    await worker.process(await worker.claim(bad))
    row = await doc_row(pool, bad)
    assert row["status"] == "failed" and "not found" in row["error"]

    # Deleting a document cascades to its chunks.
    async with pool.acquire() as conn:
        await conn.execute("DELETE FROM documents WHERE id = $1", doc)
        assert await conn.fetchval("SELECT count(*) FROM document_chunks WHERE document_id = $1", doc) == 0


async def test_lease_expiry_reclaims_crashed_jobs(pool, storage_dir):
    bot = await make_bot(pool)
    doc = await make_document(pool, bot, "md", storage_key="x/y.md")
    worker, _ = make_worker(pool, storage_dir)
    assert await worker.claim(doc)
    async with pool.acquire() as conn:  # simulate a worker that died 20 minutes ago
        await conn.execute("UPDATE documents SET locked_at = now() - interval '20 minutes' WHERE id = $1", doc)
    again = await worker.claim(doc)
    assert again and again.attempts == 2


@pytest.fixture
async def client(database_url, storage_dir):
    app = create_app(make_settings(database_url=database_url, local_storage_dir=str(storage_dir)))
    async with app.router.lifespan_context(app):
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://rag") as c:
            c.app = app
            yield c


AUTH = {"Authorization": "Bearer " + "t" * 32}


def parse_sse(body: str):
    events = []
    for block in body.strip().split("\n\n"):
        lines = dict(line.split(": ", 1) for line in block.split("\n") if ": " in line)
        events.append((lines.get("event"), json.loads(lines["data"])))
    return events


async def test_api_auth_ingest_and_chat(client, pool, storage_dir):
    assert (await client.get("/healthz")).status_code == 200
    assert (await client.post("/internal/chat", json={})).status_code == 401
    assert (await client.post("/internal/ingest/x", headers={"Authorization": "Bearer wrong"})).status_code == 401

    bot = await make_bot(pool)
    key = f"bots/{bot}/d/help.md"
    (storage_dir / key).parent.mkdir(parents=True, exist_ok=True)
    (storage_dir / key).write_text(DOC)
    doc = await make_document(pool, bot, "md", storage_key=key)
    r = await client.post(f"/internal/ingest/{doc}", headers=AUTH)
    assert r.status_code == 202
    for _ in range(100):  # the background worker picks it up
        if (await doc_row(pool, doc))["status"] == "ready":
            break
        await asyncio.sleep(0.05)
    assert (await doc_row(pool, doc))["status"] == "ready"

    req = {"bot_id": bot, "model": "claude-haiku-5-5", "message": "When is support available?",
           "fallback_message": "Please email us.", "knowledge_version": 1}
    r = await client.post("/internal/chat", json=req, headers=AUTH)
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/event-stream")
    events = parse_sse(r.text)
    names = [e for e, _ in events]
    assert names[0] == "sources" and names[-1] == "done" and "token" in names
    answer = "".join(d["t"] for e, d in events if e == "token")
    assert "Monday to Friday" in answer
    done = events[-1][1]
    assert done["cached"] is False and done["input_tokens"] > 0

    # Same first-turn question again → served from the answer cache, no tokens spent.
    events2 = parse_sse((await client.post("/internal/chat", json={**req, "message": "when is SUPPORT available"}, headers=AUTH)).text)
    assert events2[-1][1]["cached"] is True
    assert "".join(d["t"] for e, d in events2 if e == "token") == answer

    # Unknown topic → fallback message, not invented facts.
    events3 = parse_sse((await client.post("/internal/chat", json={**req, "message": "Do you sell quantum flux capacitors?"},
                                           headers=AUTH)).text)
    assert "".join(d["t"] for e, d in events3 if e == "token").strip() == "Please email us."

    r = await client.post("/internal/search", json={"bot_id": bot, "query": "refund within 30 days"}, headers=AUTH)
    assert r.json()["results"][0]["section"].endswith("Refund policy")
