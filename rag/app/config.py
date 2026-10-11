"""Service configuration, read from environment variables."""

from __future__ import annotations

from functools import lru_cache
from typing import Literal

from pydantic import Field, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=None, extra="ignore", case_sensitive=False)

    app_env: Literal["development", "production", "test"] = "development"
    log_level: str = "info"
    port: int = 8000

    database_url: str
    db_pool_min: int = 2
    db_pool_max: int = 10

    # Shared secret the Go API sends as a bearer token. The service must also be deployed
    # with internal-only ingress (Cloud Run ingress=internal).
    rag_internal_token: str = Field(min_length=24)

    # Embeddings
    embedding_provider: Literal["openai", "voyage", "hash"] = "openai"
    embedding_model: str = "text-embedding-3-small"
    embedding_dim: int = 1536
    embedding_batch_size: int = 96
    openai_api_key: str = ""
    openai_base_url: str = "https://api.openai.com/v1"
    voyage_api_key: str = ""
    voyage_base_url: str = "https://api.voyageai.com/v1"

    # LLM
    llm_provider: Literal["auto", "fake"] = "auto"  # "fake" = deterministic offline answers (dev/tests)
    anthropic_api_key: str = ""
    llm_max_output_tokens: int = 2048  # deliberate cost cap; leaves room for adaptive thinking
    llm_effort: Literal["low", "medium", "high"] = "low"
    llm_timeout_seconds: float = 90.0

    # Object storage (where the Go API puts uploaded files)
    storage_driver: Literal["local", "gcs"] = "local"
    local_storage_dir: str = "./data/uploads"
    gcs_bucket: str = ""

    redis_url: str = ""

    # Retrieval
    retrieval_top_k: int = 5
    retrieval_candidates: int = 30
    retrieval_min_similarity: float = 0.25
    rrf_k: int = 60
    hnsw_ef_search: int = 80
    fts_config: str = "english"

    # Chunking
    chunk_target_tokens: int = 500
    chunk_max_tokens: int = 600
    chunk_min_tokens: int = 120
    chunk_overlap_tokens: int = 50

    # Prompt budgets
    history_token_budget: int = 1500
    context_token_budget: int = 3000
    max_message_tokens: int = 600

    # Ingestion worker
    worker_concurrency: int = 2
    worker_poll_seconds: float = 10.0
    worker_lease_seconds: int = 600
    worker_max_attempts: int = 3
    max_fetch_bytes: int = 5 * 1024 * 1024
    fetch_timeout_seconds: float = 15.0
    fetch_trust_env: bool = False  # honour HTTP(S)_PROXY for URL scraping
    max_extract_chars: int = 2_000_000

    # Caches
    embed_cache_size: int = 5_000  # float32 vectors; ~6 KB each at 1536 dims
    answer_cache_size: int = 2_000
    answer_cache_ttl_seconds: int = 3600

    tokenizer: Literal["auto", "tiktoken", "heuristic"] = "auto"

    @field_validator("embedding_dim")
    @classmethod
    def _dim(cls, v: int) -> int:
        if not 1 <= v <= 2000:
            raise ValueError("EMBEDDING_DIM must be between 1 and 2000 (HNSW limit)")
        return v

    @property
    def is_production(self) -> bool:
        return self.app_env == "production"


@lru_cache
def get_settings() -> Settings:
    return Settings()  # type: ignore[call-arg]
