-- DeleteFacts removes every fact stored for one release. It is the first half of
-- the replace-set write: the caller deletes then re-inserts within a transaction,
-- so a replayed summarisation that yields a different fact count never leaves
-- stale rows behind.
-- name: DeleteFacts :exec
DELETE FROM mid_term_memory
WHERE user_id = @user_id AND publish_version = @publish_version;

-- InsertFact writes one atomic fact for a release. fact_index orders the facts
-- of a release; the composite key (user_id, publish_version, fact_index) makes
-- each fact its own retrievable row with its own embedding.
-- name: InsertFact :exec
INSERT INTO mid_term_memory
    (user_id, publish_version, fact_index, summary, embedding, start_seq, end_seq)
VALUES
    (@user_id, @publish_version, @fact_index, @summary, @embedding, @start_seq, @end_seq);

-- SearchMemory returns the facts most similar to the query embedding, always
-- scoped to one user. similarity is cosine similarity: the <=> operator yields
-- cosine distance, so 1 - distance restores the [0,1] scale where 1 is
-- identical. The caller applies a minimum-similarity threshold.
-- name: SearchMemory :many
SELECT
    user_id,
    publish_version,
    summary,
    start_seq,
    end_seq,
    (1 - (embedding <=> @embedding))::float8 AS similarity
FROM mid_term_memory
WHERE user_id = @user_id
ORDER BY embedding <=> @embedding
LIMIT @limit_rows;
