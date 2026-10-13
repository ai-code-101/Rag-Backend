"""Select an LLM provider from the bot's model name."""

from __future__ import annotations

from app.config import Settings
from app.llm.base import LLMError, LLMProvider
from app.llm.fake import FakeProvider
from app.llm.openai_provider import OPENROUTER_PREFIX
from app.llm.openai_provider import PREFIX as OPENAI_PREFIX


class LLMRouter:
    def __init__(self, settings: Settings):
        self.settings = settings
        self._providers: dict[str, LLMProvider] = {}

    def for_model(self, model: str) -> LLMProvider:
        if self.settings.llm_provider == "fake":
            kind = "fake"
        elif model.startswith("claude-"):
            kind = "anthropic"
        elif model.startswith(OPENAI_PREFIX):
            kind = "openai"
        elif model.startswith(OPENROUTER_PREFIX):
            kind = "openrouter"
        else:
            raise LLMError(f"no provider for model {model!r}")
        if kind not in self._providers:
            self._providers[kind] = self._build(kind)
        return self._providers[kind]

    def _build(self, kind: str) -> LLMProvider:
        s = self.settings
        if kind == "fake":
            return FakeProvider()
        if kind == "anthropic":
            from app.llm.anthropic_provider import AnthropicProvider

            return AnthropicProvider(s.anthropic_api_key, effort=s.llm_effort, timeout=s.llm_timeout_seconds)
        from app.llm.openai_provider import OpenAICompatProvider

        if kind == "openrouter":
            if not s.openrouter_api_key:
                raise LLMError("OPENROUTER_API_KEY is not configured")
            return OpenAICompatProvider(s.openrouter_api_key, s.openrouter_base_url, prefix=OPENROUTER_PREFIX,
                                        extra_headers=s.openrouter_headers, timeout=s.llm_timeout_seconds)
        return OpenAICompatProvider(s.openai_api_key, s.openai_base_url, timeout=s.llm_timeout_seconds)

    async def aclose(self) -> None:
        for p in self._providers.values():
            await p.aclose()
