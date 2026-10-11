"""Deterministic offline provider for local development and tests (LLM_PROVIDER=fake).

It "answers" by quoting the best-matching context passage, or returns the fallback
message when the context is empty, which exercises the full streaming path without
an API key."""

from __future__ import annotations

import asyncio
import re
from collections.abc import AsyncIterator

from app.llm.base import GenerateRequest, Usage
from app.prompt import NO_CONTEXT
from app.tokens import heuristic_count

_CTX = re.compile(r"<context>\n(.*?)\n</context>", re.S)


class FakeProvider:
    async def stream(self, req: GenerateRequest) -> AsyncIterator[str | Usage]:
        last = req.messages[-1]["content"]
        m = _CTX.search(last)
        context = m.group(1) if m else ""
        if not context or context == NO_CONTEXT:
            answer = req.fallback or "I don't know."
        else:
            first = context.split("\n\n", 1)[0]
            passage = first.split("\n", 1)[1] if "\n" in first else first
            answer = "According to our information: " + passage.strip()[:400]
        for word in re.findall(r"\S+\s*", answer):
            await asyncio.sleep(0)
            yield word
        prompt_tokens = heuristic_count(req.rules + req.bot_block + "".join(x["content"] for x in req.messages))
        yield Usage(input_tokens=prompt_tokens, output_tokens=heuristic_count(answer), cached_input_tokens=0, model=f"fake/{req.model}")

    async def aclose(self) -> None:
        return None
