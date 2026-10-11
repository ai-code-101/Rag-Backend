-- Core tenancy, auth and bot tables.

CREATE TABLE users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email          text NOT NULL,
    password_hash  text NOT NULL,
    email_verified boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_lower CHECK (email = lower(email))
);
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE email_otps (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   text NOT NULL,
    purpose     text NOT NULL CHECK (purpose IN ('verify_email', 'login', 'reset_password')),
    expires_at  timestamptz NOT NULL,
    attempts    integer NOT NULL DEFAULT 0,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_otps_lookup_idx ON email_otps (user_id, purpose, created_at DESC);

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id   uuid NOT NULL,
    token_hash  text NOT NULL,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid,
    user_agent  text NOT NULL DEFAULT '',
    ip          text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX refresh_tokens_hash_key ON refresh_tokens (token_hash);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

CREATE TABLE bots (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                text NOT NULL,
    public_key          text NOT NULL,
    system_instructions text NOT NULL DEFAULT '',
    fallback_message    text NOT NULL DEFAULT '',
    model               text NOT NULL,
    temperature         real NOT NULL DEFAULT 0.3 CHECK (temperature >= 0 AND temperature <= 1),
    allowed_origins     text[] NOT NULL DEFAULT '{}',
    knowledge_version   bigint NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bots_public_key_key ON bots (public_key);
CREATE INDEX bots_owner_idx ON bots (owner_id, created_at DESC);

CREATE TABLE widget_configs (
    bot_id         uuid PRIMARY KEY REFERENCES bots(id) ON DELETE CASCADE,
    theme          jsonb NOT NULL DEFAULT '{}',
    content        jsonb NOT NULL DEFAULT '{}',
    behavior       jsonb NOT NULL DEFAULT '{}',
    features       jsonb NOT NULL DEFAULT '{}',
    config_version integer NOT NULL DEFAULT 1,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE documents (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id      uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    filename    text NOT NULL,
    source_type text NOT NULL CHECK (source_type IN ('pdf', 'docx', 'txt', 'md', 'url', 'text')),
    source_url  text,
    storage_key text,
    mime_type   text NOT NULL DEFAULT '',
    size_bytes  bigint NOT NULL DEFAULT 0,
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'ready', 'failed')),
    error       text,
    checksum    text,
    chunk_count integer NOT NULL DEFAULT 0,
    attempts    integer NOT NULL DEFAULT 0,
    locked_at   timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX documents_bot_idx ON documents (bot_id, created_at DESC);
CREATE INDEX documents_bot_checksum_idx ON documents (bot_id, checksum);
-- Worker queue: only active rows are indexed, so polling stays cheap as the table grows.
CREATE INDEX documents_queue_idx ON documents (created_at) WHERE status IN ('pending', 'processing');

CREATE TABLE conversations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id          uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    visitor_id      text NOT NULL,
    channel         text NOT NULL DEFAULT 'widget' CHECK (channel IN ('widget', 'test')),
    visitor_meta    jsonb NOT NULL DEFAULT '{}',
    origin          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_message_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversations_bot_idx ON conversations (bot_id, last_message_at DESC);
CREATE INDEX conversations_visitor_idx ON conversations (bot_id, visitor_id);

CREATE TABLE messages (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id     uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    bot_id              uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    visitor_id          text NOT NULL,
    role                text NOT NULL CHECK (role IN ('user', 'assistant')),
    content             text NOT NULL,
    input_tokens        integer NOT NULL DEFAULT 0,
    output_tokens       integer NOT NULL DEFAULT 0,
    cached_input_tokens integer NOT NULL DEFAULT 0,
    model               text NOT NULL DEFAULT '',
    latency_ms          integer NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_conversation_idx ON messages (conversation_id, created_at DESC);
CREATE INDEX messages_bot_idx ON messages (bot_id, created_at DESC);

CREATE TABLE leads (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id          uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    name            text NOT NULL DEFAULT '',
    email           text NOT NULL DEFAULT '',
    phone           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX leads_bot_idx ON leads (bot_id, created_at DESC);
