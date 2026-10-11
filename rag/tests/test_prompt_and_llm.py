from app.chat import ChatRequest, answer_cache_key, normalize_question
from app.llm.anthropic_provider import FALLBACK_BETA, build_params
from app.llm.base import GenerateRequest
from app.prompt import GROUNDING_RULES, NO_CONTEXT, Turn, build_prompt, trim_history
from app.retrieval.hybrid import RetrievedChunk
from app.tokens import TokenCounter

counter = TokenCounter("heuristic")


def chunk(i: int, text: str, tokens: int = 50) -> RetrievedChunk:
    return RetrievedChunk(id=i, document_id=f"d{i}", content=text, token_count=tokens, section=f"S{i}",
                          filename="f.md", source_url=None, similarity=0.8, rrf=0.03, vec_rank=1, fts_rank=None)


def test_history_trimmed_to_budget_and_starts_with_user():
    hist = []
    for i in range(20):
        hist += [Turn("user", f"question {i} " + "x " * 40), Turn("assistant", f"answer {i} " + "y " * 40)]
    msgs, used = trim_history(hist, budget=300, per_message=200, counter=counter)
    assert used <= 300
    assert msgs[0]["role"] == "user" and msgs[-1]["role"] == "assistant"
    assert msgs[-1]["content"].startswith("answer 19")  # most recent turns kept
    roles = [m["role"] for m in msgs]
    assert all(a != b for a, b in zip(roles, roles[1:], strict=False))


def test_history_merges_same_role_and_drops_dangling_user():
    hist = [Turn("user", "a"), Turn("user", "b"), Turn("assistant", "c"), Turn("user", "unanswered")]
    msgs, _ = trim_history(hist, 1000, 200, counter)
    assert msgs == [{"role": "user", "content": "a\n\nb"}, {"role": "assistant", "content": "c"}]


def test_prompt_static_prefix_is_identical_across_requests():
    common = dict(instructions="Be a pirate.", fallback="Ask support.", counter=counter,
                  history_budget=500, context_budget=500, per_message=200)
    p1 = build_prompt(history=[], question="What are your hours?", chunks=[chunk(1, "Open 9-5.")], **common)
    p2 = build_prompt(history=[Turn("user", "hi"), Turn("assistant", "hello")], question="Refunds?",
                      chunks=[chunk(2, "30 days.")], **common)
    # The cacheable prefix (rules + bot block) must not depend on the question or context.
    assert p1.rules == p2.rules == GROUNDING_RULES
    assert p1.bot_block == p2.bot_block
    assert "Be a pirate." in p1.bot_block and "Ask support." in p1.bot_block
    assert "Open 9-5." in p1.messages[-1]["content"] and "Open 9-5." not in p1.bot_block


def test_context_budget_and_empty_context():
    chunks = [chunk(i, f"fact {i}", tokens=200) for i in range(10)]
    p = build_prompt(instructions="", fallback="", history=[], question="q", chunks=chunks, counter=counter,
                     history_budget=100, context_budget=500, per_message=100)
    assert len(p.context_chunks) == 2 and p.context_tokens <= 500
    empty = build_prompt(instructions="", fallback="", history=[], question="q", chunks=[], counter=counter,
                         history_budget=100, context_budget=500, per_message=100)
    assert NO_CONTEXT in empty.messages[-1]["content"]


def _req(model: str) -> GenerateRequest:
    return GenerateRequest(model=model, rules="R", bot_block="B", messages=[{"role": "user", "content": "q"}],
                           temperature=0.7, max_tokens=500, fallback="fb")


def test_anthropic_params_per_model():
    opus = build_params(_req("claude-opus-5-5"), "low")
    assert opus["system"][1]["cache_control"] == {"type": "ephemeral"}
    assert "cache_control" not in opus["system"][0]
    assert opus["fallbacks"] == "default" and opus["betas"] == [FALLBACK_BETA]
    assert "temperature" not in opus  # rejected by Claude 5.x models
    assert opus["output_config"] == {"effort": "low"}

    haiku = build_params(_req("claude-haiku-5-5"), "low")
    assert "fallbacks" not in haiku and "betas" not in haiku  # no server-side fallback for Haiku 5.5

    old = build_params(_req("claude-haiku-4-5"), "low")
    assert old["temperature"] == 0.7 and "output_config" not in old

    unknown = build_params(_req("claude-future-9"), "low")
    assert set(unknown) == {"model", "max_tokens", "system", "messages"}


def test_answer_cache_key():
    base = dict(bot_id="0" * 8 + "-0000-0000-0000-" + "0" * 12, model="m", message="What are your HOURS??")
    k1 = answer_cache_key(ChatRequest(**base))
    assert k1 == answer_cache_key(ChatRequest(**{**base, "message": "what are your hours"}))
    assert k1 != answer_cache_key(ChatRequest(**{**base, "knowledge_version": 1}))
    assert k1 != answer_cache_key(ChatRequest(**{**base, "system_instructions": "new"}))
    assert normalize_question("  Hello,   World! ") == "hello world"
