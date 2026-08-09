-- +goose Up
ALTER TABLE conversation
    ADD COLUMN publish_status text NOT NULL DEFAULT 'pending'
        CHECK (publish_status IN ('pending', 'published', 'promoted')),
    ADD COLUMN published_at timestamptz,
    ADD COLUMN publish_version int NOT NULL DEFAULT 0;

-- Partial: nearly every row ends up 'promoted', so excluding those keeps the
-- index small no matter how large the table grows. Serves both the claim scan
-- and the stale-reclaim sweep.
CREATE INDEX idx_conversation_publish_status
    ON conversation (user_id, seq)
    WHERE publish_status <> 'promoted';

-- +goose Down
DROP INDEX idx_conversation_publish_status;

ALTER TABLE conversation
    DROP COLUMN publish_version,
    DROP COLUMN published_at,
    DROP COLUMN publish_status;
