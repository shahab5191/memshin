-- UpsertMemory writes (or overwrites) the mid-term summary for one release. The
-- composite key (user_id, publish_version) makes a replayed or stale write an
-- in-place update, never a duplicate row.
-- name: UpsertMemory :exec
INSERT INTO mid_term_memory
    (user_id, publish_version, summary, embedding, start_seq, end_seq)
VALUES
    (@user_id, @publish_version, @summary, @embedding, @start_seq, @end_seq)
ON CONFLICT (user_id, publish_version) DO UPDATE SET
    summary    = EXCLUDED.summary,
    embedding  = EXCLUDED.embedding,
    start_seq  = EXCLUDED.start_seq,
    end_seq    = EXCLUDED.end_seq;

-- SearchMemory returns the summaries most similar to the query embedding,
-- always scoped to one user. similarity is cosine similarity: the <=> operator
-- yields cosine distance, so 1 - distance restores the [0,1] scale where 1 is
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
