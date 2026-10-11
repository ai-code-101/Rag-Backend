"""Claude via the official Anthropic SDK (streaming, prompt caching, refusal fallbacks)."""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator
from dataclasses import dataclass

import anthropic

from app.llm.base import GenerateRequest, LLMError, Usage

log = logging.getLogger(__name__)

FALLBACK_BETA = "server-side-fallback-2026-07-01"


@dataclass(frozen=True)
class ModelCaps:
    temperature: bool  # Claude 5.x models reject non-default sampling parameters
    effort: bool
    server_fallback: bool  # supports fallbacks="default"


MODEL_CAPS: dict[str, ModelCaps] = {
    "claude-opus-5-5": ModelCaps(temperature=False, effort=True, server_fallback=True),
    "claude-sonnet-5-5": ModelCaps(temperature=False, effort=True, server_fallback=True),
    "claude-haiku-5-5": ModelCaps(temperature=False, effort=True, server_fallback=False),
    "claude-sonnet-4-6": ModelCaps(temperature=True, effort=True, server_fallback=False),
    "claude-haiku-4-5": ModelCaps(temperature=True, effort=False, server_fallback=False),
}
# Unknown future models get the most conservative request shape.
DEFAULT_CAPS = ModelCaps(temperature=False, effort=False, server_fallback=False)


def build_params(req: GenerateRequest, effort: str) -> dict:
    caps = MODEL_CAPS.get(req.model, DEFAULT_CAPS)
    params: dict = {
        "model": req.model,
        "max_tokens": req.max_tokens,
        # Two system blocks: shared rules, then the bot's instructions. The breakpoint on
        # the second block caches both (provider minimums permitting) across every request
        # to this bot; history and the context-bearing last turn come after it.
        "system": [
            {"type": "text", "text": req.rules},
            {"type": "text", "text": req.bot_block, "cache_control": {"type": "ephemeral"}},
        ],
        "messages": req.messages,
    }
    if caps.temperature:
        params["temperature"] = req.temperature
    if caps.effort:
        params["output_config"] = {"effort": effort}  # low effort = fast, cheap chat answers
    if caps.server_fallback:
        params["betas"] = [FALLBACK_BETA]
        params["fallbacks"] = "default"
    return params


class AnthropicProvider:
    def __init__(self, api_key: str, effort: str = "low", timeout: float = 90.0):
        if not api_key:
            raise LLMError("ANTHROPIC_API_KEY is not configured")
        self.client = anthropic.AsyncAnthropic(api_key=api_key, timeout=timeout, max_retries=2)
        self.effort = effort

    async def stream(self, req: GenerateRequest) -> AsyncIterator[str | Usage]:
        params = build_params(req, self.effort)
        emitted = False
        try:
            async with self.client.beta.messages.stream(**params) as stream:
                async for event in stream:
                    if event.type == "content_block_delta" and event.delta.type == "text_delta":
                        if event.delta.text:
                            emitted = True
                            yield event.delta.text
                final = await stream.get_final_message()
        except anthropic.RateLimitError as exc:
            raise LLMError("model rate limited") from exc
        except anthropic.APIStatusError as exc:
            raise LLMError(f"model error {exc.status_code}") from exc
        except anthropic.APIConnectionError as exc:
            raise LLMError("model connection error") from exc

        if final.stop_reason == "refusal":
            log.warning("model refused", extra={"model": req.model, "category": getattr(final.stop_details, "category", None)})
            if not emitted:
                yield req.fallback

        u = final.usage
        cache_read = getattr(u, "cache_read_input_tokens", 0) or 0
        cache_write = getattr(u, "cache_creation_input_tokens", 0) or 0
        yield Usage(
            input_tokens=(u.input_tokens or 0) + cache_read + cache_write,
            output_tokens=u.output_tokens or 0,
            cached_input_tokens=cache_read,
            model=final.model or req.model,
        )

    async def aclose(self) -> None:
        await self.client.close()
