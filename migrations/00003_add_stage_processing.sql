-- +goose Up
-- Multi-layer promotion needs a per-row stage so a consumer only claims rows
-- released to its own layer, and mid-term's claim needs a 'processing' state
-- plus a lease timestamp for the stale-claim reclaim sweep (Phase B).
ALTER TABLE conversation
    ADD COLUMN stage      text NOT NULL DEFAULT 'mid-term',
    ADD COLUMN claimed_at timestamptz;

-- Widen the lifecycle. The original inline CHECK is auto-named, so drop and
-- re-add to include the new state.
ALTER TABLE conversation DROP CONSTRAINT conversation_publish_status_check;
ALTER TABLE conversation ADD CONSTRAINT conversation_publish_status_check
    CHECK (publish_status IN ('pending', 'published', 'processing', 'promoted'));

-- Claim hot path: the oldest unclaimed batch for a layer.
CREATE INDEX idx_conversation_claim
    ON conversation (stage, publish_status, publish_version, user_id)
    WHERE publish_status = 'published';

-- Reclaim sweep: rows stuck in 'processing' past their lease.
CREATE INDEX idx_conversation_reclaim
    ON conversation (claimed_at)
    WHERE publish_status = 'processing';

-- +goose Down
DROP INDEX idx_conversation_reclaim;
DROP INDEX idx_conversation_claim;

ALTER TABLE conversation DROP CONSTRAINT conversation_publish_status_check;
ALTER TABLE conversation ADD CONSTRAINT conversation_publish_status_check
    CHECK (publish_status IN ('pending', 'published', 'promoted'));

ALTER TABLE conversation
    DROP COLUMN claimed_at,
    DROP COLUMN stage;
