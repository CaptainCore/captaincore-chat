-- Conversations: one support thread between a customer and the operators.
-- customer_name / customer_email are snapshots from the identity token so the
-- inbox renders without calling the Manager.
CREATE TABLE conversations (
    id               INTEGER PRIMARY KEY,
    customer_user_id INTEGER NOT NULL,
    customer_name    TEXT    NOT NULL DEFAULT '',
    customer_email   TEXT    NOT NULL DEFAULT '',
    account_id       INTEGER,
    site_id          INTEGER,
    subject          TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'snoozed', 'closed')),
    snoozed_until    INTEGER,
    assignee_user_id INTEGER,
    -- Set by the first unanswered customer comment, cleared by an operator
    -- comment or a close. "Waiting on me" = status open AND waiting_since set.
    waiting_since    INTEGER,
    last_customer_at INTEGER,
    last_operator_at INTEGER,
    -- Last customer-visible comment; notes and events never bump it.
    last_message_at  INTEGER NOT NULL,
    source           TEXT    NOT NULL DEFAULT 'chat' CHECK (source IN ('chat', 'email', 'intercom')),
    intercom_id      TEXT UNIQUE,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_conversations_customer ON conversations (customer_user_id, last_message_at DESC);
CREATE INDEX idx_conversations_status ON conversations (status, last_message_at DESC);

-- Messages. Bodies are PLAIN TEXT, never HTML: clients escape on render, so
-- a customer can never inject markup into the operator view.
-- internal = 1 means operators only (every note, some events).
CREATE TABLE messages (
    id               INTEGER PRIMARY KEY,
    conversation_id  INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    kind             TEXT    NOT NULL CHECK (kind IN ('comment', 'note', 'event')),
    internal         INTEGER NOT NULL DEFAULT 0 CHECK (internal IN (0, 1)),
    author_kind      TEXT    NOT NULL CHECK (author_kind IN ('customer', 'operator', 'system')),
    author_user_id   INTEGER,
    author_name      TEXT    NOT NULL DEFAULT '',
    body             TEXT    NOT NULL,
    intercom_part_id TEXT UNIQUE,
    created_at       INTEGER NOT NULL,
    edited_at        INTEGER,
    CHECK (kind != 'note' OR internal = 1),
    CHECK (kind != 'comment' OR internal = 0)
);
CREATE INDEX idx_messages_conversation ON messages (conversation_id, id);

-- Read state per user per conversation (unread badges, "Seen" receipts,
-- and later the "email if unread after 10 minutes" timer).
CREATE TABLE reads (
    user_id              INTEGER NOT NULL,
    conversation_id      INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    last_read_message_id INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    PRIMARY KEY (user_id, conversation_id)
);
