-- Vector store. {{EMBEDDING_DIM}} is substituted from EMBEDDING_DIM when the migration runs.
-- GORM cannot describe vector columns, generated tsvector columns or HNSW indexes, so this
-- table is defined here in SQL; the Go model only maps it.

CREATE TABLE document_chunks (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    bot_id      uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    chunk_index integer NOT NULL,
    content     text NOT NULL,
    token_count integer NOT NULL,
    embedding   vector({{EMBEDDING_DIM}}) NOT NULL,
    metadata    jsonb NOT NULL DEFAULT '{}',
    tsv         tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(metadata->>'section', '')), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX document_chunks_embedding_hnsw ON document_chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX document_chunks_tsv_gin ON document_chunks USING gin (tsv);
CREATE INDEX document_chunks_bot_idx ON document_chunks (bot_id);
CREATE INDEX document_chunks_document_idx ON document_chunks (document_id, chunk_index);
