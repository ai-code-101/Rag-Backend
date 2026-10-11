"""Turn source documents into a flat list of structural blocks (headings, paragraphs,
list items, code, table rows). The chunker groups these blocks; preserving structure here
is what lets chunks follow section boundaries and carry their section titles."""

from __future__ import annotations

import io
import re
from dataclasses import dataclass, field
from typing import Literal

BlockKind = Literal["heading", "paragraph", "list", "code", "table"]


class ExtractionError(Exception):
    """The document cannot be processed (permanent failure)."""


@dataclass
class Block:
    kind: BlockKind
    text: str
    level: int = 0  # heading level (1-6)
    meta: dict = field(default_factory=dict)


_WS = re.compile(r"[ \t ]+")


def _clean(text: str) -> str:
    return _WS.sub(" ", text.replace("\r\n", "\n").replace("\r", "\n")).strip()


def decode_text(data: bytes) -> str:
    for enc in ("utf-8-sig", "utf-16"):
        try:
            return data.decode(enc)
        except UnicodeDecodeError:
            continue
    return data.decode("latin-1")


# ---- Markdown ---------------------------------------------------------------

_ATX = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
_LIST = re.compile(r"^\s*(?:[-*+]|\d+[.)])\s+")
_FENCE = re.compile(r"^\s*(```|~~~)")
_SETEXT = re.compile(r"^\s*(=+|-+)\s*$")
_MD_LINK = re.compile(r"!?\[([^\]]*)\]\([^)]*\)")
_MD_EMPH = re.compile(r"(\*\*|__|\*|_|`)(.+?)\1")


def _md_inline(text: str) -> str:
    text = _MD_LINK.sub(r"\1", text)
    return _MD_EMPH.sub(r"\2", text)


def extract_markdown(text: str) -> list[Block]:
    blocks: list[Block] = []
    para: list[str] = []
    lines = text.replace("\r\n", "\n").split("\n")

    def flush_para() -> None:
        if para:
            joined = _clean(" ".join(para))
            if joined:
                blocks.append(Block("paragraph", _md_inline(joined)))
            para.clear()

    i = 0
    while i < len(lines):
        line = lines[i]
        if _FENCE.match(line):
            flush_para()
            fence = _FENCE.match(line).group(1)  # type: ignore[union-attr]
            code: list[str] = []
            i += 1
            while i < len(lines) and not lines[i].strip().startswith(fence):
                code.append(lines[i])
                i += 1
            if code:
                blocks.append(Block("code", "\n".join(code).strip()))
            i += 1
            continue
        m = _ATX.match(line)
        if m:
            flush_para()
            blocks.append(Block("heading", _md_inline(_clean(m.group(2))), level=len(m.group(1))))
        elif para and _SETEXT.match(line) and len(para) == 1:
            level = 1 if line.strip().startswith("=") else 2
            blocks.append(Block("heading", _md_inline(_clean(para.pop())), level=level))
        elif _LIST.match(line):
            flush_para()
            item = [_LIST.sub("", line, count=1)]
            # continuation lines indented under the item
            while i + 1 < len(lines) and lines[i + 1].startswith(("  ", "\t")) and not _LIST.match(lines[i + 1]):
                i += 1
                item.append(lines[i])
            blocks.append(Block("list", _md_inline(_clean(" ".join(item)))))
        elif line.strip().startswith("|") and line.strip().endswith("|"):
            flush_para()
            cells = [c.strip() for c in line.strip().strip("|").split("|")]
            if not all(re.fullmatch(r":?-{2,}:?", c) for c in cells if c):
                blocks.append(Block("table", " | ".join(_md_inline(c) for c in cells)))
        elif not line.strip():
            flush_para()
        else:
            para.append(line.strip())
        i += 1
    flush_para()
    return blocks


# ---- Plain text -------------------------------------------------------------


def extract_text(text: str) -> list[Block]:
    blocks: list[Block] = []
    for raw in re.split(r"\n\s*\n", text.replace("\r\n", "\n")):
        lines = [ln.strip() for ln in raw.split("\n") if ln.strip()]
        if not lines:
            continue
        # A short single line without terminal punctuation followed by a paragraph reads as a heading.
        if len(lines) == 1 and len(lines[0]) <= 80 and not lines[0].endswith((".", "!", "?", ",", ";")):
            blocks.append(Block("heading", lines[0], level=2))
        elif all(_LIST.match(ln) for ln in lines):
            blocks.extend(Block("list", _clean(_LIST.sub("", ln, count=1))) for ln in lines)
        else:
            blocks.append(Block("paragraph", _clean(" ".join(lines))))
    return blocks


# ---- PDF ----------------------------------------------------------------------


def extract_pdf(data: bytes) -> list[Block]:
    from pypdf import PdfReader
    from pypdf.errors import PdfReadError

    try:
        reader = PdfReader(io.BytesIO(data))
        if reader.is_encrypted:
            try:
                reader.decrypt("")
            except Exception as exc:
                raise ExtractionError("PDF is password protected") from exc
        blocks: list[Block] = []
        for page_no, page in enumerate(reader.pages, start=1):
            text = page.extract_text() or ""
            for b in extract_text(_dehyphenate(text)):
                b.meta["page"] = page_no
                blocks.append(b)
    except PdfReadError as exc:
        raise ExtractionError(f"Could not read PDF: {exc}") from exc
    return blocks


def _dehyphenate(text: str) -> str:
    # Join words split across lines ("informa-\ntion") and single line breaks inside paragraphs.
    text = re.sub(r"(\w)-\n(\w)", r"\1\2", text)
    return re.sub(r"(?<![.!?:\n])\n(?!\n)", " ", text)


# ---- DOCX ---------------------------------------------------------------------


def extract_docx(data: bytes) -> list[Block]:
    import docx
    from docx.table import Table
    from docx.text.paragraph import Paragraph

    try:
        document = docx.Document(io.BytesIO(data))
    except Exception as exc:
        raise ExtractionError(f"Could not read DOCX: {exc}") from exc

    blocks: list[Block] = []
    body = document.element.body
    for child in body.iterchildren():
        tag = child.tag.rsplit("}", 1)[-1]
        if tag == "p":
            p = Paragraph(child, document)
            text = _clean(p.text)
            if not text:
                continue
            style = (p.style.name if p.style is not None else "") or ""
            if style.lower().startswith("heading"):
                level = int(style.split()[-1]) if style.split()[-1].isdigit() else 2
                blocks.append(Block("heading", text, level=min(level, 6)))
            elif style.lower() == "title":
                blocks.append(Block("heading", text, level=1))
            elif "list" in style.lower():
                blocks.append(Block("list", text))
            else:
                blocks.append(Block("paragraph", text))
        elif tag == "tbl":
            table = Table(child, document)
            for row in table.rows:
                cells = []
                for cell in row.cells:
                    t = _clean(cell.text)
                    if t and (not cells or cells[-1] != t):  # merged cells repeat
                        cells.append(t)
                if cells:
                    blocks.append(Block("table", " | ".join(cells)))
    return blocks


# ---- HTML ---------------------------------------------------------------------

_DROP = "script,style,noscript,template,svg,canvas,iframe,form,nav,footer,header,aside,button,select"


def extract_html(html: str) -> tuple[str, list[Block]]:
    """Returns (page title, blocks). Boilerplate (nav, footer, scripts) is removed."""
    from selectolax.lexbor import LexborHTMLParser as HTMLParser

    tree = HTMLParser(html)
    title = ""
    if tree.css_first("title") is not None:
        title = _clean(tree.css_first("title").text())  # type: ignore[union-attr]
    for node in tree.css(_DROP):
        node.decompose()
    for node in tree.css('[role="navigation"],[aria-hidden="true"],.cookie,#cookie-banner'):
        node.decompose()
    root = tree.css_first("main") or tree.css_first("article") or tree.body
    if root is None:
        return title, []

    blocks: list[Block] = []
    for node in root.traverse(include_text=False):
        tag = node.tag
        if tag in ("h1", "h2", "h3", "h4", "h5", "h6"):
            t = _clean(node.text(separator=" "))
            if t:
                blocks.append(Block("heading", t, level=int(tag[1])))
        elif tag == "p" or tag == "blockquote" or tag == "dd":
            t = _clean(node.text(separator=" "))
            if t:
                blocks.append(Block("paragraph", t))
        elif tag == "li":
            # Skip list items that only wrap nested lists (their children are emitted separately).
            t = _clean(" ".join(c.text(separator=" ") for c in node.iter(include_text=True) if c.tag not in ("ul", "ol")))
            if t:
                blocks.append(Block("list", t))
        elif tag == "pre":
            t = node.text().strip()
            if t:
                blocks.append(Block("code", t))
        elif tag == "tr":
            cells = [_clean(c.text(separator=" ")) for c in node.css("th,td")]
            cells = [c for c in cells if c]
            if cells:
                blocks.append(Block("table", " | ".join(cells)))
        elif tag == "dt":
            t = _clean(node.text(separator=" "))
            if t:
                blocks.append(Block("paragraph", t))
    # Pages built from bare <div>s: fall back to the visible text.
    if not blocks:
        blocks = extract_text(root.text(separator="\n"))
    return title, blocks


def extract(source_type: str, data: bytes, max_chars: int) -> tuple[str, list[Block]]:
    """Dispatch by source type. Returns (title, blocks)."""
    title = ""
    if source_type == "pdf":
        blocks = extract_pdf(data)
    elif source_type == "docx":
        blocks = extract_docx(data)
    elif source_type in ("md", "text"):
        blocks = extract_markdown(decode_text(data))
    elif source_type == "txt":
        blocks = extract_text(decode_text(data))
    elif source_type == "html":
        title, blocks = extract_html(decode_text(data))
    else:
        raise ExtractionError(f"unsupported source type {source_type}")
    total = 0
    out: list[Block] = []
    for b in blocks:
        total += len(b.text)
        if total > max_chars:
            break
        out.append(b)
    return title, out
