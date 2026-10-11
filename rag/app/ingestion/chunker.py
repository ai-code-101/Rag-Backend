"""Structure-aware chunking.

Blocks are packed into chunks of ~target tokens (never above max) without crossing
section boundaries when the current chunk is already a useful size. Each chunk records
its heading path, which is stored as metadata and prepended to the text that gets
embedded ("contextual chunk headers"). That improves retrieval and costs nothing at
generation time. Chunks that are split for size overlap by ~overlap tokens; chunks split at
a heading do not overlap, since the topic changes there.
"""

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass, field

from app.ingestion.extractors import Block
from app.tokens import TokenCounter

_SENTENCE = re.compile(r"(?<=[.!?])\s+(?=[A-Z0-9\"'(\[])")


@dataclass
class Chunk:
    index: int
    content: str
    token_count: int
    section: str
    heading_path: list[str]
    page: int | None = None
    meta: dict = field(default_factory=dict)

    @property
    def embed_text(self) -> str:
        return f"{self.section}\n\n{self.content}" if self.section else self.content

    @property
    def content_hash(self) -> str:
        return hashlib.sha256(self.embed_text.encode()).hexdigest()


@dataclass
class _Piece:
    text: str
    tokens: int


class Chunker:
    def __init__(self, counter: TokenCounter, target: int = 500, max_tokens: int = 600, min_tokens: int = 120, overlap: int = 50):
        if not 0 <= overlap < target <= max_tokens:
            raise ValueError("require 0 <= overlap < target <= max")
        self.counter = counter
        self.target = target
        self.max = max_tokens
        self.min = min_tokens
        self.overlap = overlap

    def _split_long(self, text: str) -> list[str]:
        """Split an oversized block on sentence, then word, boundaries."""
        sentences = _SENTENCE.split(text)
        out: list[str] = []
        cur: list[str] = []
        cur_tokens = 0
        for s in sentences:
            st = self.counter.count(s)
            if st > self.max:
                if cur:
                    out.append(" ".join(cur))
                    cur, cur_tokens = [], 0
                words = s.split()
                buf: list[str] = []
                for w in words:
                    buf.append(w)
                    if self.counter.count(" ".join(buf)) >= self.target:
                        out.append(" ".join(buf))
                        buf = []
                if buf:
                    cur, cur_tokens = [" ".join(buf)], self.counter.count(" ".join(buf))
                continue
            if cur_tokens + st > self.target and cur:
                out.append(" ".join(cur))
                cur, cur_tokens = [], 0
            cur.append(s)
            cur_tokens += st
        if cur:
            out.append(" ".join(cur))
        return out

    def _overlap_tail(self, pieces: list[_Piece]) -> list[_Piece]:
        """Last sentences of the previous chunk totalling ≈ overlap tokens."""
        if self.overlap == 0 or not pieces:
            return []
        sentences = _SENTENCE.split(pieces[-1].text)
        tail: list[str] = []
        tokens = 0
        for s in reversed(sentences):
            st = self.counter.count(s)
            if tokens + st > self.overlap and tail:
                break
            if st > self.overlap * 2:  # a single huge sentence: take its last words instead
                words = s.split()
                s = " ".join(words[-max(1, self.overlap * 3 // 4) :])
                st = self.counter.count(s)
            tail.insert(0, s)
            tokens += st
            if tokens >= self.overlap:
                break
        text = " ".join(tail)
        return [_Piece(text, self.counter.count(text))] if text else []

    def chunk(self, blocks: list[Block], doc_title: str = "") -> list[Chunk]:
        chunks: list[Chunk] = []
        path: list[tuple[int, str]] = []  # (level, heading)
        if doc_title:
            path.append((0, doc_title))
        cur: list[_Piece] = []
        cur_tokens = 0
        carry_only = False  # cur holds nothing but overlap copied from the previous chunk
        cur_path: list[str] = [h for _, h in path]
        cur_page: int | None = None

        def emit(with_overlap: bool) -> None:
            nonlocal cur, cur_tokens, carry_only
            if not cur or carry_only:
                cur, cur_tokens, carry_only = [], 0, False
                return
            content = "\n\n".join(p.text for p in cur)
            chunks.append(Chunk(
                index=len(chunks), content=content, token_count=self.counter.count(content),
                section=" > ".join(cur_path), heading_path=list(cur_path), page=cur_page,
            ))
            carry = self._overlap_tail(cur) if with_overlap else []
            cur = carry
            cur_tokens = sum(p.tokens for p in carry)
            carry_only = bool(carry)

        for b in blocks:
            page = b.meta.get("page")
            if b.kind == "heading":
                # Section boundary: flush unless the running chunk is still tiny, in which
                # case it is merged forward with the heading kept inline for context.
                if cur_tokens >= self.min or carry_only:
                    emit(with_overlap=False)
                while path and path[-1][0] >= b.level:
                    path.pop()
                path.append((b.level, b.text))
                if not cur:
                    cur_path = [h for _, h in path]
                    cur_page = page
                else:
                    t = "#" * max(1, b.level) + " " + b.text
                    cur.append(_Piece(t, self.counter.count(t)))
                    cur_tokens += cur[-1].tokens
                continue

            text = f"- {b.text}" if b.kind == "list" else b.text
            tokens = self.counter.count(text)
            pieces = [text] if tokens <= self.max else self._split_long(text)
            for ptxt in pieces:
                pt = tokens if len(pieces) == 1 else self.counter.count(ptxt)
                if not carry_only and (cur_tokens + pt > self.max or (cur_tokens >= self.target and cur_tokens + pt > self.target)):
                    emit(with_overlap=True)
                    cur_path = [h for _, h in path]
                    cur_page = page
                if carry_only and cur_tokens + pt > self.max:
                    cur, cur_tokens = [], 0  # overlap would push the chunk over the hard max
                if not cur:
                    cur_page = page
                cur.append(_Piece(ptxt, pt))
                cur_tokens += pt
                carry_only = False
        emit(with_overlap=False)
        return chunks
