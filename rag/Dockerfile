# syntax=docker/dockerfile:1.7
FROM python:3.13-slim AS base
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 \
    TIKTOKEN_CACHE_DIR=/opt/tiktoken
WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt \
    # Bake the tokenizer into the image so containers never download it at runtime.
    && python -c "import tiktoken; tiktoken.get_encoding('cl100k_base')"

COPY app ./app
RUN useradd --system --uid 10001 --no-create-home rag && chown -R rag /opt/tiktoken
USER rag

ENV PORT=8000 APP_ENV=production
EXPOSE 8000
# One process per container; scale with Cloud Run instances. The ingestion worker runs
# in-process, so deploy with CPU always allocated (and min-instances >= 1).
CMD ["sh", "-c", "exec uvicorn --factory app.main:app_factory --host 0.0.0.0 --port ${PORT} --timeout-graceful-shutdown 20 --no-server-header"]
