"""Hybrid retrieval against a real Postgres + pgvector."""

import json

from app.embeddings.providers import HashEmbedder
from app.retrieval.hybrid import HybridRetriever, RetrievalParams, RetrievedChunk, fuse_and_filter
from tests.conftest import TEST_DIM, make_bot, make_document


def rc(i, sim, rrf):
    return RetrievedChunk(id=i, document_id="d", content="", token_count=1, section="", filename="", source_url=None,
                          similarity=sim, rrf=rrf, vec_rank=None, fts_rank=None)


def test_fuse_and_filter_threshold_and_top_k():
    rows = [rc(1, 0.9, 0.02), rc(2, 0.1, 0.05), rc(3, 0.5, 0.03), rc(4, 0.6, 0.01)]
    out = fuse_and_filter(rows, RetrievalParams(top_k=2, min_similarity=0.3))
    assert [r.id for r in out] == [3, 1]  # id 2 has the best RRF but is below the threshold


async def insert_chunks(pool, bot_id, doc_id, texts, sections=None):
    emb = HashEmbedder(TEST_DIM)
    secs = sections or [""] * len(texts)
    # Embed "section + content" exactly like the ingestion worker does.
    vecs = await emb.embed([f"{s}\n\n{t}" if s else t for s, t in zip(secs, texts, strict=True)], "document")
    async with pool.acquire() as conn:
        for i, (t, v) in enumerate(zip(texts, vecs, strict=True)):
            meta = {"section": (sections or [""] * len(texts))[i]}
            await conn.execute(
                "INSERT INTO document_chunks (document_id, bot_id, chunk_index, content, token_count, embedding, metadata) "
                "VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)", doc_id, bot_id, i, t, len(t.split()), v, json.dumps(meta))


async def search(pool, bot_id, q, **kw):
    params = RetrievalParams(**{"top_k": 5, "candidates": 20, "min_similarity": 0.05, **kw})
    vec = (await HashEmbedder(TEST_DIM).embed([q], "query"))[0]
    return await HybridRetriever(pool).search(bot_id, q, vec, params)


async def test_hybrid_search_ranks_relevant_chunks_and_isolates_bots(pool):
    bot_a, bot_b = await make_bot(pool), await make_bot(pool)
    doc_a, doc_b = await make_document(pool, bot_a), await make_document(pool, bot_b)
    await insert_chunks(pool, bot_a, doc_a, [
        "Refunds are issued within 14 days of receiving the returned item.",
        "Our office opening hours are 9am to 5pm, Monday to Friday.",
        "Shipping is free for orders over 50 euros.",
    ], sections=["Refund policy", "Contact", "Shipping"])
    await insert_chunks(pool, bot_b, doc_b, ["Refunds are issued within 14 days. SECRET of bot B."])

    results = await search(pool, bot_a, "how long do refunds take?")
    assert results and results[0].content.startswith("Refunds are issued")
    assert results[0].section == "Refund policy"
    assert results[0].vec_rank == 1 and results[0].fts_rank == 1  # found by both branches
    assert all("SECRET" not in r.content for r in results)  # tenant isolation

    # Section titles are indexed (weight A): a query for the heading finds the chunk.
    contact = await search(pool, bot_a, "contact")
    assert contact[0].section == "Contact"


async def test_similarity_threshold_drops_irrelevant(pool):
    bot = await make_bot(pool)
    doc = await make_document(pool, bot)
    await insert_chunks(pool, bot, doc, ["Our espresso machines come with a two year warranty."])
    assert await search(pool, bot, "warranty espresso machines", min_similarity=0.2)
    assert await search(pool, bot, "quantum chromodynamics lecture notes", min_similarity=0.2) == []


async def test_empty_bot_and_stopword_query(pool):
    bot = await make_bot(pool)
    assert await search(pool, bot, "anything") == []
    doc = await make_document(pool, bot)
    await insert_chunks(pool, bot, doc, ["The the the a an."])
    # A query made only of stopwords produces an empty tsquery; vector branch still runs.
    await search(pool, bot, "the a an")
