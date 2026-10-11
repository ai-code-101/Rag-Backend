"""Chat pipeline: answer cache → query embedding → hybrid retrieval → prompt → stream."""

from __future__ import annotations

import hashlib
import json
import logging
import re
import time
from collections.abc import AsyncIterator
from dataclasses import dataclass

from pydantic import BaseModel, Field

from app.cache import TieredCache
from app.config import Settings
from app.embeddings.providers import CachedEmbedder
from app.llm.base import GenerateRequest, LLMError, Usage
from app.llm.router import LLMRouter
from app.prompt import Turn, build_prompt
from app.retrieval.hybrid import HybridRetriever, RetrievalParams, RetrievedChunk
from app.tokens import TokenCounter

log = logging.getLogger(__name__)


class HistoryTurn(BaseModel):
    role: str = Field(pattern="^(user|assistant)$")
    content: str = Field(max_length=20_000)


class ChatRequest(BaseModel):
    bot_id: str = Field(pattern=r"^[0-9a-f-]{36}$")
    knowledge_version: int = 0
    system_instructions: str = Field(default="", max_length=20_000)
    fallback_message: str = Field(default="", max_length=2_000)
    model: str = Field(max_length=100)
    temperature: float = Field(default=0.3, ge=0, le=1)
    history: list[HistoryTurn] = Field(default_factory=list, max_length=50)
    message: str = Field(min_length=1, max_length=8_000)
    top_k: int | None = Field(default=None, ge=1, le=20)


@dataclass
class SSE:
    event: str
    data: object

    def encode(self) -> bytes:
        return f"event: {self.event}\ndata: {json.dumps(self.data, ensure_ascii=False, separators=(',', ':'))}\n\n".encode()


_NORM = re.compile(r"[\s\W_]+", re.UNICODE)


def normalize_question(q: str) -> str:
    return _NORM.sub(" ", q.lower()).strip()


def answer_cache_key(req: ChatRequest) -> str:
    cfg = hashlib.sha256(f"{req.system_instructions}\x00{req.fallback_message}".encode()).hexdigest()[:16]
    raw = f"{req.bot_id}|{req.knowledge_version}|{req.model}|{req.temperature:.2f}|{cfg}|{normalize_question(req.message)}"
    return hashlib.sha256(raw.encode()).hexdigest()


def sources_payload(chunks: list[RetrievedChunk]) -> list[dict]:
    seen: set[str] = set()
    out = []
    for c in chunks:
        if c.document_id in seen:
            continue
        seen.add(c.document_id)
        out.append({"document_id": c.document_id, "title": c.title, "filename": c.filename,
                    "url": c.source_url, "page": c.page, "score": round(c.similarity, 3)})
    return out


class ChatService:
    def __init__(self, settings: Settings, embedder: CachedEmbedder, retriever: HybridRetriever, llms: LLMRouter,
                 answers: TieredCache, counter: TokenCounter):
        self.s = settings
        self.embedder = embedder
        self.retriever = retriever
        self.llms = llms
        self.answers = answers
        self.counter = counter

    def params(self, req: ChatRequest) -> RetrievalParams:
        return RetrievalParams(top_k=req.top_k or self.s.retrieval_top_k, candidates=self.s.retrieval_candidates,
                               min_similarity=self.s.retrieval_min_similarity, rrf_k=self.s.rrf_k, ef_search=self.s.hnsw_ef_search)

    async def retrieve(self, req: ChatRequest) -> list[RetrievedChunk]:
        vec = await self.embedder.embed_query(req.message)
        return await self.retriever.search(req.bot_id, req.message, vec, self.params(req))

    async def stream(self, req: ChatRequest) -> AsyncIterator[SSE]:
        t0 = time.monotonic()
        # Only first-turn questions are cacheable: later answers depend on the conversation.
        cache_key = answer_cache_key(req) if not req.history else None
        if cache_key:
            hit = await self.answers.get(cache_key)
            if hit:
                yield SSE("sources", hit["sources"])
                for i in range(0, len(hit["answer"]), 64):  # replay in small pieces for a natural feel
                    yield SSE("token", {"t": hit["answer"][i : i + 64]})
                yield SSE("done", {"input_tokens": 0, "output_tokens": 0, "cached_input_tokens": 0, "model": req.model, "cached": True})
                log.info("answer cache hit", extra={"bot_id": req.bot_id})
                return

        try:
            chunks = await self.retrieve(req)
        except Exception:
            log.exception("retrieval failed", extra={"bot_id": req.bot_id})
            yield SSE("error", {"code": "retrieval_failed"})
            return
        t_retrieve = time.monotonic()

        prompt = build_prompt(
            instructions=req.system_instructions, fallback=req.fallback_message,
            history=[Turn(h.role, h.content) for h in req.history], question=req.message, chunks=chunks,
            counter=self.counter, history_budget=self.s.history_token_budget,
            context_budget=self.s.context_token_budget, per_message=self.s.max_message_tokens,
        )
        sources = sources_payload(prompt.context_chunks)
        yield SSE("sources", sources)

        gen = GenerateRequest(model=req.model, rules=prompt.rules, bot_block=prompt.bot_block, messages=prompt.messages,
                              temperature=req.temperature, max_tokens=self.s.llm_max_output_tokens,
                              fallback=req.fallback_message or "Sorry, I can't help with that.")
        answer: list[str] = []
        usage = Usage(model=req.model)
        first_token: float | None = None
        try:
            provider = self.llms.for_model(req.model)
            async for item in provider.stream(gen):
                if isinstance(item, Usage):
                    usage = item
                    continue
                if first_token is None:
                    first_token = time.monotonic()
                answer.append(item)
                yield SSE("token", {"t": item})
        except LLMError as exc:
            log.error("generation failed", extra={"bot_id": req.bot_id, "model": req.model, "error": str(exc)})
            yield SSE("error", {"code": "generation_failed"})
            return

        text = "".join(answer)
        yield SSE("done", {"input_tokens": usage.input_tokens, "output_tokens": usage.output_tokens,
                           "cached_input_tokens": usage.cached_input_tokens, "model": usage.model, "cached": False})
        log.info("chat answered", extra={
            "bot_id": req.bot_id, "model": usage.model, "chunks": len(prompt.context_chunks),
            "context_tokens": prompt.context_tokens, "history_tokens": prompt.history_tokens,
            "input_tokens": usage.input_tokens, "output_tokens": usage.output_tokens,
            "cached_input_tokens": usage.cached_input_tokens,
            "retrieval_ms": int((t_retrieve - t0) * 1000),
            "ttft_ms": int(((first_token or time.monotonic()) - t0) * 1000),
            "total_ms": int((time.monotonic() - t0) * 1000),
        })
        if cache_key and text:
            await self.answers.set(cache_key, {"answer": text, "sources": sources})
