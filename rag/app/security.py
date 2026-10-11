"""Internal service authentication."""

from __future__ import annotations

import hmac

from fastapi import Header, HTTPException, Request, status


def require_internal_token(request: Request, authorization: str = Header(default="")) -> None:
    expected = request.app.state.settings.rag_internal_token
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not hmac.compare_digest(token.encode(), expected.encode()):
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="unauthorized")
