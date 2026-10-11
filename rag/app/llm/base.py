"""Provider-agnostic LLM streaming interface."""

from __future__ import annotations

from collections.abc import AsyncIterator
from dataclasses import dataclass
from typing import Protocol


class LLMError(Exception):
    pass


@dataclass
class Usage:
    input_tokens: int = 0  # total prompt tokens, including cached ones
    output_tokens: int = 0
    cached_input_tokens: int = 0
    model: str = ""


@dataclass
class GenerateRequest:
    model: str
    rules: str  # static system text shared by every bot
    bot_block: str  # per-bot system text (cached prefix ends here)
    messages: list[dict]  # alternating user/assistant, last is user
    temperature: float
    max_tokens: int
    fallback: str  # used if the provider refuses


class LLMProvider(Protocol):
    def stream(self, req: GenerateRequest) -> AsyncIterator[str | Usage]:
        """Yield text deltas, then exactly one Usage as the final item."""
        ...

    async def aclose(self) -> None: ...
