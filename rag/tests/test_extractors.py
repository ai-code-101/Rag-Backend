import io

import pytest

from app.ingestion.extractors import ExtractionError, extract, extract_html, extract_markdown, extract_text


def test_markdown_structure():
    md = """Title
=====

Intro **bold** with [a link](https://x.y).

## Steps
1. First step
2. Second step
   continues here

```python
print("code")
```

| Plan | Price |
|------|-------|
| Pro  | $10   |
"""
    blocks = extract_markdown(md)
    kinds = [(b.kind, b.text) for b in blocks]
    assert kinds[0] == ("heading", "Title")
    assert ("paragraph", "Intro bold with a link.") in kinds
    assert ("heading", "Steps") in kinds
    assert ("list", "Second step continues here") in kinds
    assert ("code", 'print("code")') in kinds
    assert ("table", "Pro | $10") in kinds
    assert not any(b.kind == "table" and "---" in b.text for b in blocks)


def test_plain_text_headings_and_lists():
    blocks = extract_text("Opening hours\n\nWe are open daily from 9 to 5.\nClosed on holidays.\n\n- Mon\n- Tue")
    assert [(b.kind, b.text) for b in blocks] == [
        ("heading", "Opening hours"),
        ("paragraph", "We are open daily from 9 to 5. Closed on holidays."),
        ("list", "Mon"),
        ("list", "Tue"),
    ]


def test_html_removes_boilerplate():
    html = """<html><head><title>FAQ | Acme</title><style>.x{}</style></head><body>
    <nav><a>Home</a><a>Pricing</a></nav>
    <main><h1>FAQ</h1><p>We ship <b>worldwide</b>.</p><ul><li>Fast</li><li>Cheap</li></ul>
    <table><tr><th>Plan</th><td>Pro</td></tr></table><script>alert(1)</script></main>
    <footer>© Acme</footer></body></html>"""
    title, blocks = extract_html(html)
    assert title == "FAQ | Acme"
    texts = [b.text for b in blocks]
    assert [t.replace(" .", ".") for t in texts] == ["FAQ", "We ship worldwide.", "Fast", "Cheap", "Plan | Pro"]
    joined = " ".join(texts)
    assert "Home" not in joined and "alert" not in joined and "©" not in joined


def test_docx_headings_and_tables():
    docx = pytest.importorskip("docx")
    d = docx.Document()
    d.add_heading("Returns", level=1)
    d.add_paragraph("You can return items within 30 days.")
    t = d.add_table(rows=1, cols=2)
    t.rows[0].cells[0].text = "Item"
    t.rows[0].cells[1].text = "Days"
    buf = io.BytesIO()
    d.save(buf)
    _, blocks = extract("docx", buf.getvalue(), 10_000)
    assert [(b.kind, b.text) for b in blocks] == [
        ("heading", "Returns"), ("paragraph", "You can return items within 30 days."), ("table", "Item | Days")]


def _minimal_pdf(text: str) -> bytes:
    stream = f"BT /F1 12 Tf 72 712 Td ({text}) Tj ET".encode()
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = io.BytesIO()
    out.write(b"%PDF-1.4\n")
    offsets = []
    for i, o in enumerate(objs, start=1):
        offsets.append(out.tell())
        out.write(f"{i} 0 obj\n".encode() + o + b"\nendobj\n")
    xref = out.tell()
    out.write(f"xref\n0 {len(objs) + 1}\n0000000000 65535 f \n".encode())
    for off in offsets:
        out.write(f"{off:010d} 00000 n \n".encode())
    out.write(f"trailer\n<< /Size {len(objs) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF".encode())
    return out.getvalue()


def test_pdf_text_and_page_numbers():
    _, blocks = extract("pdf", _minimal_pdf("Support is available around the clock."), 10_000)
    assert blocks and "Support is available around the clock." in blocks[0].text
    assert blocks[0].meta["page"] == 1


def test_bad_pdf_is_a_permanent_error():
    with pytest.raises(ExtractionError):
        extract("pdf", b"%PDF-1.4 garbage", 10_000)


def test_max_chars_cap():
    _, blocks = extract("txt", ("word " * 50 + "\n\n").encode() * 100, 1_000)
    assert sum(len(b.text) for b in blocks) <= 1_000
