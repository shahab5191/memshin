-- +goose Up
-- One row per atomic proposition, not per promoted batch. A vector covering
-- several topics sits in their centroid and is near none of them, so retrieval
-- granularity is the fact, and mid-term decomposes a batch into many rows.
CREATE TABLE mid_term_fact (
    id         uuid        PRIMARY KEY,
    user_id    text        NOT NULL,
    content    text        NOT NULL,
    embedding  vector(768) NOT NULL,

    -- Provenance back to the messages this was extracted from. Kept so a fact
    -- can be traced to the exchange that produced it, and so consolidation can
    -- later tell which of two contradictory facts came from where.
    source_turn_ids uuid[]  NOT NULL,

    -- (publish_version, fact_index) identifies a fact within its release. The
    -- release is rewritten wholesale on redelivery rather than merged, so the
    -- index only has to be unique within one batch.
    publish_version int     NOT NULL,
    fact_index      int     NOT NULL,

    -- When the fact became true, which is not when we heard about it: "I moved
    -- to Oslo last year" is ingested today. NULL until extraction is confident
    -- enough to fill it; readers fall back to created_at.
    valid_from   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Set by consolidation once a later fact is judged to replace this one.
    -- NULL means current, and search filters on it, so the column works before
    -- anything writes to it.
    superseded_at timestamptz,

    -- Lexical half of hybrid retrieval. Dense search misses rare tokens —
    -- names, identifiers, "shellfish" — which is exactly where memory content
    -- carries its value. Generated so it cannot drift from content.
    content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED
);

-- Redelivery of a promotion doorbell must not double-write a release.
CREATE UNIQUE INDEX mid_term_fact_batch_idx
    ON mid_term_fact (user_id, publish_version, fact_index);

-- Every read is scoped to one user, and a single user's fact count stays small
-- enough that an exact scan beats an approximate index while keeping perfect
-- recall. HNSW becomes worth its recall loss only once one user's partition is
-- large; nothing here has to change when that day comes.
CREATE INDEX mid_term_fact_user_idx
    ON mid_term_fact (user_id)
    WHERE superseded_at IS NULL;

CREATE INDEX mid_term_fact_tsv_idx ON mid_term_fact USING gin (content_tsv);

-- +goose Down
DROP TABLE mid_term_fact;
