"""OpenRouter integration against a mocked API (no network)."""

import json

import httpx
import pytest

from app.embeddings.providers import build_embedder, embedding_cache
from app.llm.base import GenerateRequest, LLMError, Usage
from app.llm.router import LLMRouter
from tests.conftest import make_settings

KEY = "sk-or-v1-test"


def settings(**kw):
    return make_settings(**{"llm_provider": "auto", "openrouter_api_key": KEY, "openrouter_site_url": "https://app.example.com", **kw})


def sse(*chunks: dict | str) -> bytes:
    lines = []
    for c in chunks:
        lines.append(c if isinstance(c, str) else "data: " + json.dumps(c))
        lines.append("")
    return ("\n".join(lines) + "\n").encode()


REQ = GenerateRequest(model="openrouter:openai/gpt-4o-mini", rules="R", bot_block="B",
                      messages=[{"role": "user", "content": "hi"}], temperature=0.2, max_tokens=100, fallback="fb")


async def collect(provider, req=REQ):
    text, usage = [], None
    async for item in provider.stream(req):
        if isinstance(item, Usage):
            usage = item
        else:
            text.append(item)
    return "".join(text), usage


async def test_chat_stream_request_and_parsing():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["headers"] = request.headers
        seen["body"] = json.loads(request.content)
        body = sse(
            ": OPENROUTER PROCESSING",  # keep-alive comment lines must be ignored
            {"choices": [{"delta": {"content": "Hel"}}]},
            {"choices": [{"delta": {"content": "lo"}}]},
            {"choices": [], "model": "openai/gpt-4o-mini",
             "usage": {"prompt_tokens": 42, "completion_tokens": 2, "prompt_tokens_details": {"cached_tokens": 30}}},
            "data: [DONE]",
        )
        return httpx.Response(200, content=body, headers={"content-type": "text/event-stream"})

    router = LLMRouter(settings())
    provider = router.for_model(REQ.model)
    provider._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    text, usage = await collect(provider)

    assert text == "Hello"
    assert usage == Usage(input_tokens=42, output_tokens=2, cached_input_tokens=30, model="openai/gpt-4o-mini")
    assert seen["url"] == "https://openrouter.ai/api/v1/chat/completions"
    assert seen["headers"]["authorization"] == f"Bearer {KEY}"
    assert seen["headers"]["x-title"] == "Horizon Valley"
    assert seen["headers"]["http-referer"] == "https://app.example.com"
    body = seen["body"]
    assert body["model"] == "openai/gpt-4o-mini"  # "openrouter:" prefix stripped
    assert body["stream"] is True and body["messages"][0]["role"] == "system"


async def test_chat_stream_errors():
    def mid_stream_error(request):
        return httpx.Response(200, content=sse({"choices": [{"delta": {"content": "x"}}]},
                                               {"error": {"message": "upstream overloaded"}}))

    def http_error(request):
        return httpx.Response(402, json={"error": {"message": "insufficient credits"}})

    for handler in (mid_stream_error, http_error):
        provider = LLMRouter(settings()).for_model(REQ.model)
        provider._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        with pytest.raises(LLMError):
            await collect(provider)


def test_missing_key_and_unknown_prefix():
    with pytest.raises(LLMError):
        LLMRouter(settings(openrouter_api_key="")).for_model(REQ.model)
    with pytest.raises(LLMError):
        LLMRouter(settings()).for_model("mystery-model")


async def test_openrouter_embeddings():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["auth"] = request.headers["authorization"]
        seen["body"] = json.loads(request.content)
        n = len(seen["body"]["input"])
        return httpx.Response(200, json={"data": [{"index": i, "embedding": [0.5] * 8} for i in reversed(range(n))]})

    s = settings(embedding_provider="openrouter", embedding_model="openai/text-embedding-3-small", embedding_dim=8)
    emb = build_embedder(s, embedding_cache(s))
    emb.inner._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    vecs = await emb.embed(["a", "b"], "document")

    assert len(vecs) == 2 and len(vecs[0]) == 8
    assert seen["url"] == "https://openrouter.ai/api/v1/embeddings"
    assert seen["auth"] == f"Bearer {KEY}"
    assert seen["body"] == {"model": "openai/text-embedding-3-small", "input": ["a", "b"], "dimensions": 8}
