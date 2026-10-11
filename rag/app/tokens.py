"""Token counting.

tiktoken's cl100k encoding is a close-enough proxy for budgeting across providers. If it
is unavailable (no network to fetch the encoding and none cached) a calibrated
heuristic is used; budgets are approximate by nature.
"""

from __future__ import annotations

import logging
import math
import re
from functools import lru_cache

log = logging.getLogger(__name__)

_WORD_RE = re.compile(r"\w+|[^\w\s]", re.UNICODE)


class TokenCounter:
    def __init__(self, mode: str = "auto"):
        self._enc = None
        if mode in ("auto", "tiktoken"):
            try:
                import tiktoken

                self._enc = tiktoken.get_encoding("cl100k_base")
            except Exception as exc:  # network blocked, encoding not cached, ...
                if mode == "tiktoken":
                    raise
                log.warning("tiktoken unavailable, using heuristic token counts", extra={"error": str(exc)})

    @property
    def exact(self) -> bool:
        return self._enc is not None

    def count(self, text: str) -> int:
        if not text:
            return 0
        if self._enc is not None:
            return len(self._enc.encode(text, disallowed_special=()))
        return heuristic_count(text)


def heuristic_count(text: str) -> int:
    # Roughly: one token per short word/punctuation mark, long words split ~every 4 chars.
    total = 0
    for w in _WORD_RE.findall(text):
        total += max(1, math.ceil(len(w) / 4)) if len(w) > 4 else 1
    return total


@lru_cache
def default_counter(mode: str = "auto") -> TokenCounter:
    return TokenCounter(mode)
