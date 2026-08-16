-- name: InsertFact :exec
INSERT INTO mid_term_fact (
    id, user_id, content, embedding, source_turn_ids,
    publish_version, fact_index, valid_from
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- DeleteFactsForVersion clears a release before it is rewritten. Extraction is
-- not deterministic, so a redelivered batch may yield a different number of
-- facts than the attempt that died before acknowledging; replacing the release
-- wholesale keeps that from leaving orphans behind, which ON CONFLICT would.
-- name: DeleteFactsForVersion :exec
DELETE FROM mid_term_fact
WHERE user_id = @user_id AND publish_version = @publish_version::int;

-- SearchFacts runs every probe of a decomposed query in one round trip and
-- fuses the results.
--
-- Two retrievers run per probe because they fail in opposite directions: dense
-- search generalises over paraphrase but is weak on rare tokens, lexical search
-- is the reverse. Their scores are not comparable, so they are combined by rank
-- with reciprocal rank fusion rather than by any attempt to calibrate the two
-- scales against each other.
--
-- A fact surfaced by several probes accumulates several reciprocal-rank terms
-- and rises accordingly, which is the behaviour we want: agreement across
-- independent probes is evidence.
-- name: SearchFacts :many
WITH probes AS (
    -- Two single-argument unnests joined on ordinality rather than one
    -- two-argument call, which sqlc's catalog cannot resolve.
    SELECT v.idx, v.vec, t.txt
    FROM unnest(@vectors::vector[]) WITH ORDINALITY AS v(vec, idx)
    JOIN unnest(@texts::text[])     WITH ORDINALITY AS t(txt, idx) USING (idx)
),
dense AS (
    SELECT p.idx,
           m.id,
           m.distance,
           row_number() OVER (PARTITION BY p.idx ORDER BY m.distance) AS rank
    FROM probes p
    CROSS JOIN LATERAL (
        SELECT f.id, (f.embedding <=> p.vec) AS distance
        FROM mid_term_fact f
        WHERE f.user_id = @user_id
          AND f.superseded_at IS NULL
          -- Applied per probe, before fusion: retrieving the k least-unrelated
          -- facts for a question memory has nothing to say about is how an
          -- irrelevant memory gets stated back to the user as fact.
          AND (f.embedding <=> p.vec) <= @max_distance::float8
        ORDER BY f.embedding <=> p.vec
        LIMIT @per_probe::int
    ) m
),
lexical AS (
    SELECT p.idx,
           m.id,
           row_number() OVER (PARTITION BY p.idx ORDER BY m.score DESC) AS rank
    FROM probes p
    CROSS JOIN LATERAL (
        SELECT f.id, ts_rank(f.content_tsv, websearch_to_tsquery('english', p.txt)) AS score
        FROM mid_term_fact f
        WHERE f.user_id = @user_id
          AND f.superseded_at IS NULL
          AND f.content_tsv @@ websearch_to_tsquery('english', p.txt)
        ORDER BY score DESC
        LIMIT @per_probe::int
    ) m
),
fused AS (
    SELECT r.id,
           -- The constant damps the influence of the top rank so a single
           -- retriever's first hit cannot outweigh agreement further down.
           sum(1.0 / (60 + r.rank)) AS score,
           -- 2 is the upper bound of cosine distance, so a fact the dense
           -- retriever never saw reads as maximally distant, which is exactly
           -- what it is from that retriever's point of view.
           coalesce(min(r.distance), 2) AS best_distance
    FROM (
        SELECT idx, id, rank, distance FROM dense
        UNION ALL
        SELECT idx, id, rank, NULL::float8 FROM lexical
    ) r
    GROUP BY r.id
)
SELECT f.id,
       f.content,
       f.valid_from,
       f.created_at,
       fu.score::float8         AS score,
       fu.best_distance::float8 AS best_distance
FROM fused fu
JOIN mid_term_fact f ON f.id = fu.id
ORDER BY fu.score DESC, f.created_at DESC
LIMIT @max_results::int;
