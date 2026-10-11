"""Two-level cache: an in-process LRU with TTL, optionally backed by Redis so that all
instances share embeddings and answers."""

from __future__ import annotations

import json
import logging
import time
from collections import OrderedDict
from collections.abc import Callable
from typing import Any

log = logging.getLogger(__name__)


class LRUCache:
    def __init__(self, maxsize: int, ttl: float | None = None):
        self.maxsize = maxsize
        self.ttl = ttl
        self._data: OrderedDict[str, tuple[float, Any]] = OrderedDict()
        self.hits = 0
        self.misses = 0

    def get(self, key: str) -> Any | None:
        item = self._data.get(key)
        if item is None:
            self.misses += 1
            return None
        expires, value = item
        if expires and expires < time.monotonic():
            del self._data[key]
            self.misses += 1
            return None
        self._data.move_to_end(key)
        self.hits += 1
        return value

    def set(self, key: str, value: Any) -> None:
        expires = time.monotonic() + self.ttl if self.ttl else 0.0
        self._data[key] = (expires, value)
        self._data.move_to_end(key)
        while len(self._data) > self.maxsize:
            self._data.popitem(last=False)

    def __len__(self) -> int:
        return len(self._data)


class TieredCache:
    """LRU in front of optional Redis. Redis failures degrade to LRU-only."""

    def __init__(self, namespace: str, maxsize: int, ttl: int | None, redis: Any | None = None,
                 to_json: Callable[[Any], Any] | None = None, from_json: Callable[[Any], Any] | None = None):
        self.ns = namespace
        self.ttl = ttl
        self.lru = LRUCache(maxsize, ttl)
        self.redis = redis
        # Optional converters between the in-memory representation and a JSON-able one.
        self._to_json = to_json or (lambda v: v)
        self._from_json = from_json or (lambda v: v)

    async def get(self, key: str) -> Any | None:
        v = self.lru.get(key)
        if v is not None or self.redis is None:
            return v
        try:
            raw = await self.redis.get(f"{self.ns}:{key}")
        except Exception as exc:
            log.warning("redis get failed", extra={"error": str(exc)})
            return None
        if raw is None:
            return None
        v = self._from_json(json.loads(raw))
        self.lru.set(key, v)
        return v

    async def get_many(self, keys: list[str]) -> list[Any | None]:
        out = [self.lru.get(k) for k in keys]
        missing = [i for i, v in enumerate(out) if v is None]
        if self.redis is not None and missing:
            try:
                raws = await self.redis.mget([f"{self.ns}:{keys[i]}" for i in missing])
            except Exception as exc:
                log.warning("redis mget failed", extra={"error": str(exc)})
                raws = [None] * len(missing)
            for i, raw in zip(missing, raws, strict=True):
                if raw is not None:
                    out[i] = self._from_json(json.loads(raw))
                    self.lru.set(keys[i], out[i])
        return out

    async def set(self, key: str, value: Any) -> None:
        self.lru.set(key, value)
        if self.redis is not None:
            try:
                await self.redis.set(f"{self.ns}:{key}", json.dumps(self._to_json(value)), ex=self.ttl)
            except Exception as exc:
                log.warning("redis set failed", extra={"error": str(exc)})

    async def set_many(self, items: dict[str, Any]) -> None:
        for k, v in items.items():
            self.lru.set(k, v)
        if self.redis is not None and items:
            try:
                pipe = self.redis.pipeline()
                for k, v in items.items():
                    pipe.set(f"{self.ns}:{k}", json.dumps(self._to_json(v)), ex=self.ttl)
                await pipe.execute()
            except Exception as exc:
                log.warning("redis pipeline failed", extra={"error": str(exc)})
