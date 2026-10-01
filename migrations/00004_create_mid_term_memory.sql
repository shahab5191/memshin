-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE mid_term_memory (
    user_id         text        NOT NULL,
    publish_version int         NOT NULL,
    summary         text        NOT NULL,
    embedding       vector(768) NOT NULL,
    start_seq       bigint      NOT NULL,
    end_seq         bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, publish_version)
);

CREATE INDEX mid_term_memory_user_idx ON mid_term_memory (user_id);

CREATE INDEX mid_term_memory_embedding_idx ON mid_term_memory
    USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE mid_term_memory;
DROP EXTENSION vector;
