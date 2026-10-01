-- GetFocus returns the latest subject for a user, or no row when they have
-- none. Recency is last_turn_seq DESC with id DESC as the tiebreak: extractions
-- finish out of order, so a stale one may carry a higher id, but the highest
-- last_turn_seq always marks the subject that is current.
-- name: GetFocus :one
SELECT subject
FROM focus
WHERE user_id = @user_id
ORDER BY last_turn_seq DESC, id DESC
LIMIT 1;

-- InsertFocus appends a subject row. Focus is append-only: every change writes
-- a new row stamped with the turn it became current, never an upsert, so the
-- history of subjects survives.
-- name: InsertFocus :exec
INSERT INTO focus (user_id, subject, last_turn_seq)
VALUES (@user_id, @subject, @last_turn_seq);
