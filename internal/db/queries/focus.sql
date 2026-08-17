-- ExpireFocusSession clears a user's focus once the whole set has gone cold.
--
-- All-or-nothing is what makes this a session boundary rather than a decay
-- curve. Inside one long conversation a topic last mentioned forty minutes ago
-- is still under discussion, so per-item expiry would quietly retire live
-- context; once every item is stale there is no conversation left to belong to.
--
-- It deletes rather than filters so a returning user starts genuinely clean:
-- hits and first_seen_at cannot carry over from a conversation that ended days
-- ago and silently outrank whatever is raised next.
-- name: ExpireFocusSession :execrows
DELETE FROM focus_item AS o
WHERE o.user_id = @user_id
  AND NOT EXISTS (
      SELECT 1
      FROM focus_item f
      WHERE f.user_id = @user_id
        AND f.last_touched_at > now() - make_interval(secs => @idle_seconds::int)
  );

-- CurrentFocus needs no staleness predicate: ExpireFocusSession has already
-- decided whether this session exists at all, so recency here only orders the
-- set, it does not retire anything from it.
-- name: CurrentFocus :many
SELECT phrase
FROM focus_item
WHERE user_id = @user_id
ORDER BY last_touched_at DESC, hits DESC, phrase
LIMIT @cap::int;

-- ReinforceFocus upserts the topics the last turn was about. A phrase already
-- present is refreshed and counted rather than duplicated, which is what lets a
-- recurring topic outlive an incidental one without any explicit topic tracking.
--
-- DISTINCT because ON CONFLICT DO UPDATE cannot touch the same row twice in one
-- statement, and an extractor naming a topic twice is not an error worth failing
-- the write over.
-- name: ReinforceFocus :exec
INSERT INTO focus_item (user_id, phrase)
SELECT DISTINCT @user_id, p
FROM unnest(@phrases::text[]) AS p
ON CONFLICT (user_id, phrase) DO UPDATE
SET hits            = focus_item.hits + 1,
    last_touched_at = now();

-- PruneFocus enforces the working-memory cap. A focus set of twenty is just a
-- worse short-term window, so anything past the cap is dropped by recency.
--
-- The tiebreak is not decoration: one reinforcement statement stamps every row
-- it touches with the same now(), so ordering by timestamp alone would make
-- which item survives arbitrary.
-- name: PruneFocus :execrows
DELETE FROM focus_item AS o
WHERE o.user_id = @user_id
  AND o.phrase NOT IN (
      SELECT f.phrase
      FROM focus_item f
      WHERE f.user_id = @user_id
      ORDER BY f.last_touched_at DESC, f.hits DESC, f.phrase
      LIMIT @cap::int
  );
