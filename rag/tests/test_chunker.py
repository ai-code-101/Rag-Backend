from app.ingestion.chunker import Chunker
from app.ingestion.extractors import Block, extract_markdown
from app.tokens import TokenCounter

counter = TokenCounter("heuristic")


def para(n_words: int, word: str = "lorem") -> str:
    # ~1 token per short word with the heuristic counter; sentences every 12 words.
    words = [f"{word}{i % 7}" for i in range(n_words)]
    out = []
    for i in range(0, n_words, 12):
        out.append(" ".join(words[i : i + 12]).capitalize() + ".")
    return " ".join(out)


def test_respects_headings_and_records_section_path():
    md = f"""# Guide

## Pricing

{para(150)}

## Refunds

{para(150, "refund")}

### Exceptions

{para(140, "except")}
"""
    chunks = Chunker(counter, target=500, max_tokens=600, min_tokens=100, overlap=50).chunk(extract_markdown(md))
    assert [c.section for c in chunks] == ["Guide > Pricing", "Guide > Refunds", "Guide > Refunds > Exceptions"]
    assert "refund" not in chunks[0].content  # no bleed across sections
    assert chunks[1].embed_text.startswith("Guide > Refunds\n\n")
    assert [c.index for c in chunks] == [0, 1, 2]


def test_size_bounds_and_overlap_within_a_section():
    blocks = [Block("heading", "Manual", level=1)] + [Block("paragraph", para(120, f"w{i}x")) for i in range(12)]
    chunks = Chunker(counter, target=500, max_tokens=600, min_tokens=100, overlap=50).chunk(blocks)
    assert len(chunks) >= 3
    for c in chunks:
        assert c.token_count <= 600, c.token_count
        assert c.section == "Manual"
    for c in chunks[:-1]:
        assert c.token_count >= 350  # packed close to target, not tiny fragments
    # Consecutive chunks of the same section share ~50 tokens of overlap.
    for a, b in zip(chunks, chunks[1:], strict=False):
        tail = a.content.split(". ")[-1].strip(". ")
        assert tail and tail in b.content
        overlap_tokens = counter.count(b.content.split("\n\n")[0])
        assert 10 <= overlap_tokens <= 100


def test_tiny_sections_are_merged_forward():
    md = "## A\n\nShort intro.\n\n## B\n\nAnother short bit.\n\n## C\n\n" + para(100)
    chunks = Chunker(counter, target=500, max_tokens=600, min_tokens=100, overlap=50).chunk(extract_markdown(md))
    assert len(chunks) == 1
    assert "## B" in chunks[0].content and "## C" in chunks[0].content
    assert chunks[0].section == "A"


def test_oversized_paragraph_is_split_on_sentences():
    blocks = [Block("paragraph", para(1500))]
    chunks = Chunker(counter, target=500, max_tokens=600, min_tokens=100, overlap=50).chunk(blocks)
    assert len(chunks) >= 3
    assert all(c.token_count <= 600 for c in chunks)
    assert all(c.content.rstrip().endswith(".") for c in chunks)


def test_hash_changes_only_with_content():
    c1 = Chunker(counter).chunk([Block("heading", "H", 1), Block("paragraph", "Same text here.")])[0]
    c2 = Chunker(counter).chunk([Block("heading", "H", 1), Block("paragraph", "Same text here.")])[0]
    c3 = Chunker(counter).chunk([Block("heading", "H", 1), Block("paragraph", "Different text.")])[0]
    assert c1.content_hash == c2.content_hash != c3.content_hash


def test_page_numbers_are_kept():
    blocks = [Block("paragraph", para(400), meta={"page": 3}), Block("paragraph", para(400), meta={"page": 4})]
    chunks = Chunker(counter, target=500, max_tokens=600, min_tokens=100, overlap=0).chunk(blocks)
    assert chunks[0].page == 3 and chunks[-1].page == 4
