-- +goose Up
CREATE TABLE focus (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       text        NOT NULL,
    subject       text        NOT NULL,
    last_turn_seq bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX focus_user_latest_idx ON focus (user_id, last_turn_seq DESC);

-- +goose Down
DROP TABLE focus;
