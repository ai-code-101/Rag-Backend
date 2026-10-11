"""Internal endpoints (called only by the Go API)."""

from __future__ import annotations

import re

from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

from app.chat import SSE, ChatRequest
from app.security import require_internal_token

router = APIRouter(prefix="/internal", dependencies=[Depends(require_internal_token)])

_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


@router.post("/ingest/{document_id}", status_code=202)
async def ingest(document_id: str, request: Request) -> dict:
    if not _UUID.match(document_id):
        raise HTTPException(status_code=404, detail="not found")
    request.app.state.worker.wake()
    return {"status": "queued"}


@router.post("/chat")
async def chat(req: ChatRequest, request: Request) -> StreamingResponse:
    service = request.app.state.chat

    async def events():
        # Starlette cancels this generator when the client disconnects, which closes the
        # upstream model stream, so abandoned answers stop consuming tokens.
        try:
            async for ev in service.stream(req):
                yield ev.encode()
        except Exception:
            request.app.state.log.exception("chat stream crashed", extra={"bot_id": req.bot_id})
            yield SSE("error", {"code": "internal"}).encode()

    return StreamingResponse(events(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})


class SearchRequest(BaseModel):
    bot_id: str = Field(pattern=r"^[0-9a-f-]{36}$")
    query: str = Field(min_length=1, max_length=2000)
    top_k: int = Field(default=5, ge=1, le=20)


@router.post("/search")
async def search(req: SearchRequest, request: Request) -> dict:
    """Retrieval only, for debugging relevance and for tests."""
    service = request.app.state.chat
    creq = ChatRequest(bot_id=req.bot_id, model="n/a", message=req.query, top_k=req.top_k)
    chunks = await service.retrieve(creq)
    return {"results": [
        {"id": c.id, "document_id": c.document_id, "section": c.section, "content": c.content,
         "similarity": round(c.similarity, 4), "rrf": round(c.rrf, 5), "vec_rank": c.vec_rank, "fts_rank": c.fts_rank}
        for c in chunks
    ]}
