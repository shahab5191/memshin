-- +goose Up
-- Focus is working memory: the handful of things under discussion right now,
-- which is what lets a reference like "that" or "the other one" be resolved
-- into something worth searching for.
--
-- It is volatile, but volatility is a lifetime rule rather than a storage
-- choice. Keeping it here instead of in process memory costs a sub-millisecond
-- read on a turn that already spends hundreds of milliseconds decomposing, and
-- buys a focus set that survives a restart and is the same across replicas.
CREATE TABLE focus_item (
    user_id text NOT NULL,
    phrase  text NOT NULL,

    -- Reinforcement. A topic raised in four of the last five turns and one
    -- mentioned once should not have equal standing, and the count is what
    -- distinguishes them.
    hits int NOT NULL DEFAULT 1,

    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_touched_at timestamptz NOT NULL DEFAULT now(),

    -- The phrase is the identity: re-mentioning a topic reinforces the existing
    -- row rather than adding a near-duplicate beside it.
    PRIMARY KEY (user_id, phrase)
);

CREATE INDEX focus_item_recent_idx ON focus_item (user_id, last_touched_at DESC);

-- +goose Down
DROP TABLE focus_item;
