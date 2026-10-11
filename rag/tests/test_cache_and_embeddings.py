import math

from app.cache import LRUCache, TieredCache
from app.embeddings.providers import CachedEmbedder, HashEmbedder, embedding_cache
from tests.conftest import make_settings


def test_lru_eviction_and_ttl(monkeypatch):
    c = LRUCache(2)
    c.set("a", 1)
    c.set("b", 2)
    c.get("a")
    c.set("c", 3)
    assert c.get("b") is None and c.get("a") == 1 and c.get("c") == 3

    import app.cache as cache_mod

    now = [1000.0]
    monkeypatch.setattr(cache_mod.time, "monotonic", lambda: now[0])
    t = LRUCache(10, ttl=5)
    t.set("k", "v")
    now[0] += 6
    assert t.get("k") is None


class CountingEmbedder(HashEmbedder):
    def __init__(self, dim):
        super().__init__(dim)
        self.calls: list[list[str]] = []

    async def embed(self, texts, input_type):
        self.calls.append(list(texts))
        return await super().embed(texts, input_type)


async def test_cached_embedder_only_embeds_misses():
    inner = CountingEmbedder(32)
    emb = CachedEmbedder(inner, embedding_cache(make_settings()))
    v1 = await emb.embed(["alpha", "beta", "alpha"], "document")
    assert inner.calls == [["alpha", "beta"]]  # de-duplicated
    assert v1[0] == v1[2]
    await emb.embed(["beta", "gamma"], "document")
    assert inner.calls[-1] == ["gamma"]
    await emb.embed(["gamma"], "query")  # query and document embeddings are cached separately
    assert inner.calls[-1] == ["gamma"]
    assert all(isinstance(x, float) for x in v1[0])


async def test_hash_embedder_is_normalised_and_lexical():
    e = HashEmbedder(256)
    a, b, c = await e.embed(["refund policy for orders", "what is your refund policy", "office opening hours"], "document")

    def cos(x, y):
        return sum(i * j for i, j in zip(x, y, strict=True))

    assert math.isclose(cos(a, a), 1.0, rel_tol=1e-6)
    assert cos(a, b) > cos(a, c)


async def test_tiered_cache_survives_redis_errors():
    class Broken:
        async def get(self, *_):
            raise ConnectionError("down")

        async def set(self, *_, **__):
            raise ConnectionError("down")

    c = TieredCache("x", 10, 60, redis=Broken())
    await c.set("k", {"v": 1})
    assert await c.get("k") == {"v": 1}
    assert await c.get("missing") is None
