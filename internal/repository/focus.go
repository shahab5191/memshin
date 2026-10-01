package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

type Focus struct {
	q *sqlc.Queries
}

func NewFocus(pool *pgxpool.Pool) *Focus {
	return &Focus{q: sqlc.New(pool)}
}

// GetFocus returns the user's latest subject, or the empty string when they
// have none. No rows is not an error: a first turn has no subject yet.
func (f *Focus) GetFocus(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("get focus: empty user id")
	}

	subject, err := f.q.GetFocus(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get focus: %w", err)
	}

	return subject, nil
}

// InsertFocus appends a subject row for the turn it became current. Append-only:
// always a new row, never an upsert, so the history of subjects survives.
func (f *Focus) InsertFocus(ctx context.Context, userID, subject string, lastTurnSeq int64) error {
	if userID == "" {
		return fmt.Errorf("insert focus: empty user id")
	}
	if subject == "" {
		return fmt.Errorf("insert focus: empty subject")
	}

	err := f.q.InsertFocus(ctx, sqlc.InsertFocusParams{
		UserID:      userID,
		Subject:     subject,
		LastTurnSeq: lastTurnSeq,
	})
	if err != nil {
		return fmt.Errorf("insert focus: %w", err)
	}

	return nil
}
