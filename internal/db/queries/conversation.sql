-- AppendTurn writes every message of a turn as one COPY, which is a single
-- statement and therefore atomic on its own — no explicit transaction needed
-- regardless of how many messages the turn contains. created_at is omitted so
-- Postgres applies its DEFAULT now().
-- name: AppendTurn :copyfrom
INSERT INTO conversation (id, user_id, turn_id, role, content)
VALUES ($1, $2, $3, $4, $5);

-- ClaimPromotable selects and marks in one statement. The
-- publish_status = 'pending' predicate makes it a compare-and-swap, so only
-- rows this caller actually transitioned are returned and no message is handed
-- out twice.
-- name: ClaimPromotable :many
UPDATE conversation
SET publish_status = 'published',
    published_at   = now()
WHERE user_id = @user_id
  AND publish_status = 'pending'
RETURNING seq, id, user_id, turn_id, role, content, created_at;

-- RecentByUser returns the newest @limit_count messages in chronological
-- order. The inner query walks conversation_user_recent_idx backwards; the
-- outer one flips the window so callers get it oldest-first for the prompt.
-- name: RecentByUser :many
SELECT seq, id, user_id, turn_id, role, content, created_at
FROM (
    SELECT seq, id, user_id, turn_id, role, content, created_at
    FROM conversation
    WHERE user_id = @user_id
        AND publish_status <> 'promoted'
    ORDER BY seq DESC
    LIMIT sqlc.narg('limit_count')
) recent
ORDER BY seq ASC;
