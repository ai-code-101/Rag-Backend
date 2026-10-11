"""Hybrid retrieval: pgvector cosine similarity + Postgres full-text search, merged with
Reciprocal Rank Fusion. One SQL round trip; every branch is filtered by bot_id."""

from __future__ import annotations

import json
from dataclasses import dataclass, field

import asyncpg

# The query embedding is $1, bot_id $2, per-branch candidate count $3, the raw query text
# $4 and the RRF constant $5. The FTS branch ORs the query's lexemes, because natural-language
# questions rarely contain every term ("plainto" ANDs them), and lets ts_rank_cd order matches.
HYBRID_SQL = """
WITH q AS (
    SELECT nullif(replace(plainto_tsquery({cfg}, $4)::text, '&', '|'), '')::tsquery AS query
),
vec AS (
    SELECT id, row_number() OVER (ORDER BY dist) AS rnk
    FROM (
        SELECT id, embedding <=> $1 AS dist
        FROM document_chunks
        WHERE bot_id = $2
        ORDER BY embedding <=> $1
        LIMIT $3
    ) v
),
fts AS (
    SELECT id, row_number() OVER (ORDER BY score DESC) AS rnk
    FROM (
        SELECT c.id, ts_rank_cd(c.tsv, q.query, 32) AS score
        FROM document_chunks c, q
        WHERE c.bot_id = $2 AND q.query IS NOT NULL AND c.tsv @@ q.query
        ORDER BY score DESC
        LIMIT $3
    ) f
),
fused AS (
    SELECT coalesce(vec.id, fts.id) AS id,
           coalesce(1.0 / ($5 + vec.rnk), 0) + coalesce(1.0 / ($5 + fts.rnk), 0) AS rrf,
           vec.rnk AS vec_rank, fts.rnk AS fts_rank
    FROM vec FULL OUTER JOIN fts ON vec.id = fts.id
)
SELECT c.id, c.document_id, c.content, c.token_count, c.metadata,
       d.filename, d.source_url, d.source_type,
       1 - (c.embedding <=> $1) AS similarity,
       f.rrf, f.vec_rank, f.fts_rank
FROM fused f
JOIN document_chunks c ON c.id = f.id
JOIN documents d ON d.id = c.document_id
WHERE c.bot_id = $2
ORDER BY f.rrf DESC
"""


@dataclass
class RetrievedChunk:
    id: int
    document_id: str
    content: str
    token_count: int
    section: str
    filename: str
    source_url: str | None
    similarity: float
    rrf: float
    vec_rank: int | None
    fts_rank: int | None
    page: int | None = None
    meta: dict = field(default_factory=dict)

    @property
    def title(self) -> str:
        return self.section or self.filename


@dataclass
class RetrievalParams:
    top_k: int = 5
    candidates: int = 30
    min_similarity: float = 0.25
    rrf_k: int = 60
    ef_search: int = 80


def fuse_and_filter(rows: list[RetrievedChunk], params: RetrievalParams) -> list[RetrievedChunk]:
    """Apply the similarity threshold to the RRF-ordered candidates and keep top_k.

    The threshold uses the true cosine similarity of every candidate (keyword-only matches
    included), so lexical hits on unrelated chunks cannot sneak into the prompt."""
    kept = [r for r in rows if r.similarity >= params.min_similarity]
    kept.sort(key=lambda r: (r.rrf, r.similarity), reverse=True)
    return kept[: params.top_k]


class HybridRetriever:
    def __init__(self, pool: asyncpg.Pool, fts_config: str = "english", iterative_scan: bool = False):
        if not fts_config.isidentifier():
            raise ValueError("invalid FTS config name")
        self.pool = pool
        self.sql = HYBRID_SQL.format(cfg=f"'{fts_config}'::regconfig")
        self.iterative_scan = iterative_scan

    async def search(self, bot_id: str, query_text: str, query_vec: list[float], params: RetrievalParams) -> list[RetrievedChunk]:
        async with self.pool.acquire() as conn, conn.transaction(readonly=True):
            # Higher ef_search = better recall for filtered HNSW scans; iterative scans
            # (pgvector >= 0.8) keep scanning until enough rows pass the bot_id filter.
            await conn.execute(f"SET LOCAL hnsw.ef_search = {int(params.ef_search)}")
            if self.iterative_scan:
                await conn.execute("SET LOCAL hnsw.iterative_scan = relaxed_order")
            records = await conn.fetch(self.sql, query_vec, bot_id, params.candidates, query_text, params.rrf_k)
        rows = []
        for r in records:
            meta = r["metadata"]
            if isinstance(meta, str):
                meta = json.loads(meta)
            rows.append(RetrievedChunk(
                id=r["id"], document_id=str(r["document_id"]), content=r["content"], token_count=r["token_count"],
                section=meta.get("section", ""), filename=r["filename"], source_url=r["source_url"],
                similarity=float(r["similarity"]), rrf=float(r["rrf"]), vec_rank=r["vec_rank"], fts_rank=r["fts_rank"],
                page=meta.get("page"), meta=meta,
            ))
        return fuse_and_filter(rows, params)
