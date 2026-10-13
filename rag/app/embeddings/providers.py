"""Embedding providers behind one interface, plus batching, retries and caching."""

from __future__ import annotations

import asyncio
import hashlib
import logging
import math
import random
import re
from array import array
from typing import Literal, Protocol

import httpx

from app.cache import TieredCache

log = logging.getLogger(__name__)

InputType = Literal["document", "query"]


class EmbeddingError(Exception):
    pass


class Embedder(Protocol):
    model: str
    dim: int

    async def embed(self, texts: list[str], input_type: InputType) -> list[list[float]]: ...

    async def aclose(self) -> None: ...


async def _post_with_retry(client: httpx.AsyncClient, url: str, payload: dict, headers: dict, attempts: int = 4) -> dict:
    delay = 1.0
    for attempt in range(1, attempts + 1):
        try:
            resp = await client.post(url, json=payload, headers=headers)
        except httpx.TransportError as exc:
            if attempt == attempts:
                raise EmbeddingError(f"embedding request failed: {exc}") from exc
        else:
            if resp.status_code < 300:
                return resp.json()
            if resp.status_code not in (408, 409, 429) and resp.status_code < 500:
                raise EmbeddingError(f"embedding provider returned {resp.status_code}: {resp.text[:300]}")
            if attempt == attempts:
                raise EmbeddingError(f"embedding provider returned {resp.status_code}")
            retry_after = resp.headers.get("retry-after")
            if retry_after and retry_after.replace(".", "", 1).isdigit():
                delay = max(delay, float(retry_after))
        await asyncio.sleep(delay + random.uniform(0, delay / 2))  # noqa: S311 (retry jitter)
        delay *= 2
    raise EmbeddingError("unreachable")


class OpenAIEmbedder:
    """Any OpenAI-compatible /embeddings endpoint (OpenAI, Azure-compatible gateways, Ollama, …)."""

    def __init__(self, api_key: str, base_url: str, model: str, dim: int, batch_size: int, extra_headers: dict[str, str] | None = None):
        self.model, self.dim, self.batch_size = model, dim, batch_size
        self._url = base_url.rstrip("/") + "/embeddings"
        self._headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._headers.update({k: v for k, v in (extra_headers or {}).items() if v})
        self._client = httpx.AsyncClient(timeout=httpx.Timeout(60.0, connect=5.0))

    async def embed(self, texts: list[str], input_type: InputType) -> list[list[float]]:
        out: list[list[float]] = []
        for i in range(0, len(texts), self.batch_size):
            batch = texts[i : i + self.batch_size]
            payload: dict = {"model": self.model, "input": batch}
            if "text-embedding-3" in self.model:  # also matches OpenRouter's "openai/text-embedding-3-small"
                payload["dimensions"] = self.dim  # Matryoshka truncation: smaller index, same model
            data = await _post_with_retry(self._client, self._url, payload, self._headers)
            vectors = [d["embedding"] for d in sorted(data["data"], key=lambda d: d["index"])]
            out.extend(vectors)
        return out

    async def aclose(self) -> None:
        await self._client.aclose()


class VoyageEmbedder:
    """Voyage AI embeddings (distinguishes query vs document inputs)."""

    def __init__(self, api_key: str, base_url: str, model: str, dim: int, batch_size: int):
        self.model, self.dim, self.batch_size = model, dim, min(batch_size, 128)
        self._url = base_url.rstrip("/") + "/embeddings"
        self._headers = {"Authorization": f"Bearer {api_key}"}
        self._client = httpx.AsyncClient(timeout=httpx.Timeout(60.0, connect=5.0))

    async def embed(self, texts: list[str], input_type: InputType) -> list[list[float]]:
        out: list[list[float]] = []
        for i in range(0, len(texts), self.batch_size):
            payload = {"model": self.model, "input": texts[i : i + self.batch_size], "input_type": input_type, "output_dimension": self.dim}
            data = await _post_with_retry(self._client, self._url, payload, self._headers)
            out.extend(d["embedding"] for d in sorted(data["data"], key=lambda d: d["index"]))
        return out

    async def aclose(self) -> None:
        await self._client.aclose()


_TOKEN = re.compile(r"[a-z0-9]+")
_STOP = frozenset(
    "a an and are as at be by for from has have how i in is it of on or our the this to was we what when where which who why will with you your".split()
)


def _stem(w: str) -> str:
    for suf in ("ing", "ed", "es", "s"):
        if len(w) > len(suf) + 2 and w.endswith(suf):
            return w[: -len(suf)]
    return w


class HashEmbedder:
    """Deterministic, dependency-free embeddings via feature hashing of stemmed words.
    Gives meaningful lexical similarity for local development and tests without any API
    key. Not for production quality."""

    def __init__(self, dim: int):
        self.model, self.dim = "hash-v1", dim

    def _vec(self, text: str) -> list[float]:
        words = [_stem(w) for w in _TOKEN.findall(text.lower()) if w not in _STOP]
        v = [0.0] * self.dim
        for f in words:
            h = int.from_bytes(hashlib.blake2b(f.encode(), digest_size=8).digest(), "big")
            v[h % self.dim] += 1.0 if (h >> 63) & 1 else -1.0
        norm = math.sqrt(sum(x * x for x in v)) or 1.0
        return [x / norm for x in v]

    async def embed(self, texts: list[str], input_type: InputType) -> list[list[float]]:
        return [self._vec(t) for t in texts]

    async def aclose(self) -> None:
        return None


class CachedEmbedder:
    """Wraps a provider with a content-addressed cache. Only cache misses are sent to the
    provider, in batches, so re-ingesting a mostly unchanged document and repeated queries
    are almost free."""

    def __init__(self, inner: Embedder, cache: TieredCache):
        self.inner = inner
        self.cache = cache
        self.model, self.dim = inner.model, inner.dim

    def _key(self, text: str, input_type: InputType) -> str:
        return hashlib.sha256(f"{self.inner.model}|{self.dim}|{input_type}|{text}".encode()).hexdigest()

    async def embed(self, texts: list[str], input_type: InputType) -> list[list[float]]:
        if not texts:
            return []
        keys = [self._key(t, input_type) for t in texts]
        cached = await self.cache.get_many(keys)
        missing = [i for i, v in enumerate(cached) if v is None]
        if missing:
            # De-duplicate identical texts within the batch.
            uniq: dict[str, int] = {}
            for i in missing:
                uniq.setdefault(keys[i], i)
            fresh = await self.inner.embed([texts[i] for i in uniq.values()], input_type)
            if len(fresh) != len(uniq):
                raise EmbeddingError("provider returned wrong number of embeddings")
            for vec in fresh:
                if len(vec) != self.dim:
                    raise EmbeddingError(f"provider returned dimension {len(vec)}, expected {self.dim}")
            # float32 arrays: ~4 bytes per dimension instead of ~32 for a list of Python floats.
            new = {k: array("f", v) for k, v in zip(uniq.keys(), fresh, strict=True)}
            await self.cache.set_many(new)
            for i in missing:
                cached[i] = new[keys[i]]
        return [list(v) for v in cached]  # type: ignore[arg-type]

    async def embed_query(self, text: str) -> list[float]:
        return (await self.embed([text], "query"))[0]

    async def aclose(self) -> None:
        await self.inner.aclose()


def embedding_cache(settings, redis=None) -> TieredCache:
    return TieredCache("emb", settings.embed_cache_size, ttl=7 * 24 * 3600, redis=redis,
                       to_json=list, from_json=lambda v: array("f", v))


def build_embedder(settings, cache: TieredCache) -> CachedEmbedder:
    if settings.embedding_provider == "openai":
        inner: Embedder = OpenAIEmbedder(settings.openai_api_key, settings.openai_base_url, settings.embedding_model,
                                         settings.embedding_dim, settings.embedding_batch_size)
    elif settings.embedding_provider == "openrouter":
        inner = OpenAIEmbedder(settings.openrouter_api_key, settings.openrouter_base_url, settings.embedding_model,
                               settings.embedding_dim, settings.embedding_batch_size, extra_headers=settings.openrouter_headers)
    elif settings.embedding_provider == "voyage":
        inner = VoyageEmbedder(settings.voyage_api_key, settings.voyage_base_url, settings.embedding_model,
                               settings.embedding_dim, settings.embedding_batch_size)
    else:
        inner = HashEmbedder(settings.embedding_dim)
    return CachedEmbedder(inner, cache)
