-- +goose Up
CREATE TABLE conversation (
    -- seq is the ordering key. UUIDv7 is time-ordered across milliseconds but
    -- guarantees nothing *within* one, so the two messages of a single turn
    -- could sort wrong. seq is assigned in insert order, always.
    seq        bigint      GENERATED ALWAYS AS IDENTITY NOT NULL,
    id         uuid        PRIMARY KEY,
    user_id    text        NOT NULL,
    turn_id    uuid        NOT NULL,
    role       text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Hot path: the last N messages for one user.
CREATE INDEX conversation_user_recent_idx ON conversation (user_id, seq DESC);

-- Mid-term hydration: every message belonging to a promoted turn.
CREATE INDEX conversation_turn_idx ON conversation (turn_id);

-- +goose Down
DROP TABLE conversation;
