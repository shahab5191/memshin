-- name: AppendTurn :copyfrom
INSERT INTO conversation (id, user_id, turn_id, role, content)
VALUES ($1, $2, $3, $4, $5);

-- ReleaseBatch hands a bounded, versioned batch of backlog to mid-term. It is
-- a tap that pours a fixed measure, not the old dam that held everything back:
-- nothing here waits for an earlier release to be acknowledged, because batches
-- are versioned and consumed oldest-first, so several may be in flight at once.
--
-- A release fires only once a full batch has accumulated above the floor, so
-- mid-term receives whole stretches (eight messages) rather than one exchange
-- per turn, and never an unbounded backlog at once.
-- name: ReleaseBatch :one
WITH backlog AS (
    SELECT b.seq
    FROM conversation b
    WHERE b.user_id = @user_id
      AND b.publish_status = 'pending'
),
batch AS (
    SELECT seq
    FROM backlog
    -- A full batch must be available above the floor before anything is
    -- released, so mid-term is never handed a couple of messages per turn.
    WHERE (SELECT count(*) FROM backlog) - @recent_floor::bigint >= @batch_size::bigint
    ORDER BY seq ASC
    LIMIT @batch_size::bigint
),
released AS (
    UPDATE conversation c
    SET publish_status  = 'published',
        published_at    = now(),
        stage           = @stage,
        -- One version for the whole release, not per row. Taking the user's
        -- high-water mark gives the batch a single value to be acknowledged
        -- under, and lets the claim query pick the oldest unclaimed batch.
        publish_version = (
            SELECT coalesce(max(v.publish_version), 0) + 1
            FROM conversation v
            WHERE v.user_id = @user_id
        )
    WHERE c.seq IN (SELECT batch.seq FROM batch)
      -- Re-evaluated under the row lock: if a concurrent turn for this user
      -- released the same batch first, the row is no longer pending and is
      -- skipped instead of being published twice.
      AND c.publish_status = 'pending'
    RETURNING c.seq
)
SELECT count(*)::bigint AS released FROM released;

-- ClaimBatch atomically claims the oldest unclaimed batch for a layer. The
-- claim is the mutual-exclusion point: the UPDATE transitions the rows to
-- 'processing' under the row lock, so two workers racing for the same user get
-- exactly one batch between them — the loser's RETURNING is empty and it exits
-- without a summarisation.
-- name: ClaimBatch :many
WITH oldest AS (
    SELECT min(publish_version) AS version
    FROM conversation
    WHERE user_id = @user_id
      AND stage = @stage
      AND publish_status = 'published'
)
UPDATE conversation c
SET publish_status = 'processing',
    claimed_at      = now()
FROM oldest
WHERE c.user_id = @user_id
  AND c.stage = @stage
  AND c.publish_status = 'published'
  AND c.publish_version = oldest.version
RETURNING c.seq, c.id, c.user_id, c.turn_id, c.role, c.content, c.created_at, c.publish_version;

-- MarkPromoted acknowledges the turns a downstream layer has durably stored,
-- dropping them out of the short-term window.
--
-- The version fences the write. A worker whose lease expired mid-summary has
-- had its batch reclaimed and republished under a higher version, so its late
-- acknowledgement matches no rows rather than promoting messages that another
-- worker now owns. Zero rows affected means exactly that, and is not an error.
-- name: MarkPromoted :execrows
UPDATE conversation
SET publish_status = 'promoted'
WHERE user_id = @user_id
  AND turn_id = ANY(@turn_ids::uuid[])
  AND stage = @stage
  AND publish_status = 'processing'
  AND publish_version = @publish_version::int;

-- Short term memory window (4 exchange and anything not persisted in mid-term memory)
-- name: ShortTermWindow :many
WITH tail AS (
    SELECT t.turn_id
    FROM conversation t
    WHERE t.user_id = @user_id
    ORDER BY t.seq DESC
    LIMIT @recent_count
),
cutoff AS (
    SELECT min(w.seq) AS seq
    FROM conversation w
    WHERE w.user_id = @user_id
      AND w.turn_id IN (SELECT tail.turn_id FROM tail)
)
SELECT c.seq, c.id, c.user_id, c.turn_id, c.role, c.content, c.created_at
FROM conversation c
WHERE c.user_id = @user_id
  AND (
        c.publish_status <> 'promoted'
        -- NULL when the user has no history at all; the comparison then yields
        -- NULL and this half simply contributes nothing.
     OR c.seq >= (SELECT cutoff.seq FROM cutoff)
  )
ORDER BY c.seq ASC;

-- Short term memory latest turn messages
-- name: LatestTurn :many
SELECT c.seq, c.id, c.user_id, c.turn_id, c.role, c.content, c.created_at
FROM conversation c
WHERE c.user_id = @user_id
  AND c.stage = 'latest'
  AND c.publish_status = 'published'
ORDER BY c.seq ASC;
