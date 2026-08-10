-- name: AppendTurn :copyfrom
INSERT INTO conversation (id, user_id, turn_id, role, content)
VALUES ($1, $2, $3, $4, $5);

-- ClaimPromotable releases the backlog above the recent floor to mid-term. It
-- is a dam, not a tap: two conditions must hold before anything is let go.
--
-- The backlog must have grown past @threshold, so mid-term receives whole
-- stretches of conversation instead of one exchange per turn. And nothing this
-- user published earlier may still be unacknowledged, because a release that
-- never lands must not be followed by a later one — mid-term can recover from
-- being behind, but not from a hole in the middle of its history.
--
-- Everything above the floor goes at once, uncapped. How much fits in one
-- summarisation is mid-term's business, and it acknowledges in whatever
-- chunks suit it.
-- name: ClaimPromotable :one
WITH backlog AS (
    SELECT b.seq
    FROM conversation b
    WHERE b.user_id = @user_id
      AND b.publish_status = 'pending'
),
batch AS (
    SELECT seq
    FROM backlog
    -- All-or-nothing gate: under the threshold, or with a release still
    -- outstanding, this yields no rows and the update below is a no-op.
    WHERE (SELECT count(*) FROM backlog) >= @threshold::bigint
      AND NOT EXISTS (
          SELECT 1
          FROM conversation p
          WHERE p.user_id = @user_id
            AND p.publish_status = 'published'
      )
    ORDER BY seq ASC
    -- The newest @recent_floor messages stay pending, so the live thread is
    -- never handed away mid-conversation.
    --
    -- greatest() is not decoration. Postgres evaluates LIMIT even when the
    -- gate above has already excluded every row, so a backlog shorter than the
    -- floor — every conversation's first turn — would otherwise abort the
    -- statement with "LIMIT must not be negative".
    LIMIT greatest((SELECT count(*) FROM backlog) - @recent_floor::bigint, 0)
),
claimed AS (
    UPDATE conversation c
    SET publish_status  = 'published',
        published_at    = now(),
        -- One version for the whole release, not per row. A batch can mix rows
        -- that were reclaimed from an earlier release with rows that have never
        -- left, and those carry different counters; taking the user's high-water
        -- mark gives the batch a single value to be acknowledged under.
        publish_version = (
            SELECT coalesce(max(v.publish_version), 0) + 1
            FROM conversation v
            WHERE v.user_id = @user_id
        )
    WHERE c.seq IN (SELECT batch.seq FROM batch)
      -- Re-evaluated under the row lock: if a concurrent turn for this user
      -- claimed the same batch first, the row is no longer pending and is
      -- skipped instead of being published twice.
      AND c.publish_status = 'pending'
    RETURNING c.seq
)
SELECT count(*)::bigint AS released FROM claimed;

-- PublishedBatch is what mid-term reads when the doorbell rings: everything
-- released to it and not yet acknowledged. publish_version travels with the
-- rows because acknowledging requires it.
-- name: PublishedBatch :many
SELECT seq, id, user_id, turn_id, role, content, created_at, publish_version
FROM conversation
WHERE user_id = @user_id
  AND publish_status = 'published'
ORDER BY seq ASC;

-- MarkPromoted acknowledges the turns mid-term has durably stored, dropping
-- them out of the short-term window.
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
  AND publish_status = 'published'
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
