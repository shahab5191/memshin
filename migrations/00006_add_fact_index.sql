-- +goose Up
-- A batch can now yield several atomic facts, so one row per release is no
-- longer enough. fact_index disambiguates facts within a release, keeping the
-- (user_id, publish_version) prefix as the batch key while allowing one row per
-- fact. Existing single-summary rows keep fact_index 0.
ALTER TABLE mid_term_memory DROP CONSTRAINT mid_term_memory_pkey;
ALTER TABLE mid_term_memory ADD COLUMN fact_index int NOT NULL DEFAULT 0;
ALTER TABLE mid_term_memory ADD PRIMARY KEY (user_id, publish_version, fact_index);

-- +goose Down
ALTER TABLE mid_term_memory DROP CONSTRAINT mid_term_memory_pkey;
ALTER TABLE mid_term_memory DROP COLUMN fact_index;
ALTER TABLE mid_term_memory ADD PRIMARY KEY (user_id, publish_version);
