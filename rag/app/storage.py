"""Read access to the object storage the Go API writes uploads into."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Protocol


class StorageError(Exception):
    pass


class ObjectReader(Protocol):
    async def read(self, key: str, max_bytes: int) -> bytes: ...


def _valid_key(key: str) -> bool:
    if not key or key.startswith("/") or "\\" in key:
        return False
    return all(p not in ("", ".", "..") for p in key.split("/"))


class LocalReader:
    def __init__(self, root: str):
        self.root = Path(root).resolve()

    async def read(self, key: str, max_bytes: int) -> bytes:
        if not _valid_key(key):
            raise StorageError("invalid storage key")
        path = (self.root / key).resolve()
        if self.root not in path.parents:
            raise StorageError("invalid storage key")

        def _read() -> bytes:
            with path.open("rb") as f:
                data = f.read(max_bytes + 1)
            return data

        try:
            data = await asyncio.to_thread(_read)
        except FileNotFoundError as exc:
            raise StorageError("source file not found") from exc
        if len(data) > max_bytes:
            raise StorageError("source file too large")
        return data


class GCSReader:
    def __init__(self, bucket: str):
        from google.cloud import storage  # imported lazily: heavy and only needed in production

        self._bucket = storage.Client().bucket(bucket)

    async def read(self, key: str, max_bytes: int) -> bytes:
        if not _valid_key(key):
            raise StorageError("invalid storage key")

        def _read() -> bytes:
            blob = self._bucket.get_blob(key)
            if blob is None:
                raise StorageError("source file not found")
            if blob.size and blob.size > max_bytes:
                raise StorageError("source file too large")
            return blob.download_as_bytes()

        return await asyncio.to_thread(_read)
