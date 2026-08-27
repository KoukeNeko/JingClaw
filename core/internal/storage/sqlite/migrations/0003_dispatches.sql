-- The delivery outbox.
--
-- Posting to a platform is not the same act as recording an event, and it
-- succeeds or fails separately, so it gets its own durable queue rather than
-- riding on the session log. The sequence here is deliberately distinct from a
-- session's event sequence and from whatever sequence a platform's own
-- protocol uses; conflating the three leaves nobody able to say what a given
-- number refers to.
CREATE TABLE gateway_dispatches (
    id          TEXT    PRIMARY KEY,

    -- Monotonic per gateway account, which is the cursor a gateway resumes
    -- from after a disconnect.
    seq         INTEGER NOT NULL,
    account_id  TEXT    NOT NULL,

    session_id  TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id      TEXT    NOT NULL DEFAULT '',

    -- Where it goes, as an opaque conversation reference the adapter decodes.
    target      TEXT    NOT NULL,

    kind        TEXT    NOT NULL,
    payload     TEXT    NOT NULL,

    created_at  INTEGER NOT NULL,

    -- Set once the platform has confirmed it. Anything unacknowledged is
    -- redelivered, so a gateway that dies mid-post does not silently drop the
    -- reply.
    delivered_at INTEGER,

    -- What the platform called the messages it created, so a later edit can
    -- find them.
    platform_message_ids TEXT NOT NULL DEFAULT '[]'
) STRICT;

CREATE UNIQUE INDEX gateway_dispatches_seq ON gateway_dispatches (account_id, seq);

CREATE INDEX gateway_dispatches_undelivered ON gateway_dispatches (account_id, seq)
    WHERE delivered_at IS NULL;

-- Inbound deduplication.
--
-- Platforms redeliver on reconnect. Without this, one dropped connection turns
-- a single request into several runs, each doing the same work.
CREATE TABLE gateway_inbound (
    idempotency_key TEXT    PRIMARY KEY,
    account_id      TEXT    NOT NULL,
    session_id      TEXT    NOT NULL,
    run_id          TEXT    NOT NULL DEFAULT '',
    received_at     INTEGER NOT NULL
) STRICT;

-- An operator's decision that a conversation may reach a workspace.
CREATE TABLE gateway_bindings (
    id                 TEXT    PRIMARY KEY,

    platform           TEXT    NOT NULL,
    account_id         TEXT    NOT NULL,
    tenant_id          TEXT    NOT NULL DEFAULT '',
    channel_id         TEXT    NOT NULL,

    workspace_id       TEXT    NOT NULL,
    permission_profile TEXT    NOT NULL DEFAULT 'gateway',

    allowed_principals TEXT    NOT NULL DEFAULT '[]',
    allowed_claims     TEXT    NOT NULL DEFAULT '[]',

    created_at         INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX gateway_bindings_channel
    ON gateway_bindings (platform, account_id, tenant_id, channel_id);

-- Which conversation a session belongs to, so a message continues its thread
-- rather than starting a new session every time.
CREATE TABLE gateway_conversations (
    conversation_key TEXT    PRIMARY KEY,
    session_id       TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    binding_id       TEXT    NOT NULL,
    created_at       INTEGER NOT NULL
) STRICT;
