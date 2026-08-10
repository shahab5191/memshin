-- AppendTurn writes every message of a turn as one COPY, which is a single
-- statement and therefore atomic on its own — no explicit transaction needed
-- regardless of how many messages the turn contains. created_at is omitted so
-- Postgres applies its DEFAULT now().
-- name: AppendTurn :copyfrom
INSERT INTO conversation (id, user_id, turn_id, role, content)
VALUES ($1, $2, $3, $4, $5);

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
