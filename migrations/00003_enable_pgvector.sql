-- +goose Up
-- Owned by a migration rather than the image's init scripts, which only run on
-- an empty volume and so would skip every existing database.
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down
DROP EXTENSION vector;
