"""OpenAI-compatible chat completions (OpenAI, Together, Groq, vLLM, Ollama, …).

Bots select these models with an ``openai:`` prefix, e.g. ``openai:gpt-4.1-mini``.
"""

from __future__ import annotations

import json
from collections.abc import AsyncIterator

import httpx

from app.llm.base import GenerateRequest, LLMError, Usage

PREFIX = "openai:"


class OpenAICompatProvider:
    def __init__(self, api_key: str, base_url: str, timeout: float = 90.0):
        self._url = base_url.rstrip("/") + "/chat/completions"
        self._headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._client = httpx.AsyncClient(timeout=httpx.Timeout(timeout, connect=5.0))

    async def stream(self, req: GenerateRequest) -> AsyncIterator[str | Usage]:
        model = req.model.removeprefix(PREFIX)
        # Static system text first: OpenAI-style providers cache identical prompt prefixes automatically.
        messages = [{"role": "system", "content": req.rules + "\n\n" + req.bot_block}, *req.messages]
        payload = {
            "model": model,
            "messages": messages,
            "temperature": req.temperature,
            "max_tokens": req.max_tokens,
            "stream": True,
            "stream_options": {"include_usage": True},
        }
        usage = Usage(model=model)
        try:
            async with self._client.stream("POST", self._url, json=payload, headers=self._headers) as resp:
                if resp.status_code >= 400:
                    body = (await resp.aread())[:300]
                    raise LLMError(f"model error {resp.status_code}: {body!r}")
                async for line in resp.aiter_lines():
                    if not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if data == "[DONE]":
                        break
                    chunk = json.loads(data)
                    for choice in chunk.get("choices") or []:
                        text = (choice.get("delta") or {}).get("content")
                        if text:
                            yield text
                    if chunk.get("usage"):
                        u = chunk["usage"]
                        usage = Usage(
                            input_tokens=u.get("prompt_tokens", 0),
                            output_tokens=u.get("completion_tokens", 0),
                            cached_input_tokens=(u.get("prompt_tokens_details") or {}).get("cached_tokens", 0),
                            model=chunk.get("model", model),
                        )
        except httpx.HTTPError as exc:
            raise LLMError(f"model connection error: {exc.__class__.__name__}") from exc
        yield usage

    async def aclose(self) -> None:
        await self._client.aclose()
