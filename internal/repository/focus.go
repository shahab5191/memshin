package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

type Focus struct {
	q *sqlc.Queries
}

func NewFocus(pool *pgxpool.Pool) *Focus {
	return &Focus{q: sqlc.New(pool)}
}

// Current returns the topics under discussion, ending the session first if the
// whole set has gone cold. It reports whether a session was ended, which is
// worth logging: an unexpected reset is the difference between focus working
// and focus silently being empty on every turn.
func (f *Focus) Current(
	ctx context.Context,
	userID string,
	idleGap time.Duration,
	cap int,
) ([]string, bool, error) {
	if userID == "" {
		return nil, false, fmt.Errorf("current focus: empty user id")
	}
	if cap <= 0 {
		return nil, false, fmt.Errorf("current focus: cap %d must be positive", cap)
	}

	expired, err := f.q.ExpireFocusSession(ctx, sqlc.ExpireFocusSessionParams{
		UserID:      userID,
		IdleSeconds: int32(idleGap.Seconds()),
	})
	if err != nil {
		return nil, false, fmt.Errorf("current focus: expire session: %w", err)
	}

	phrases, err := f.q.CurrentFocus(ctx, sqlc.CurrentFocusParams{
		UserID: userID,
		Cap:    int32(cap),
	})
	if err != nil {
		return nil, false, fmt.Errorf("current focus: %w", err)
	}

	return phrases, expired > 0, nil
}

// Reinforce records the topics the latest turn was about and trims the set back
// to the cap.
func (f *Focus) Reinforce(ctx context.Context, userID string, phrases []string, cap int) error {
	if userID == "" {
		return fmt.Errorf("reinforce focus: empty user id")
	}
	if cap <= 0 {
		return fmt.Errorf("reinforce focus: cap %d must be positive", cap)
	}
	if len(phrases) == 0 {
		return nil
	}

	if err := f.q.ReinforceFocus(ctx, sqlc.ReinforceFocusParams{
		UserID:  userID,
		Phrases: phrases,
	}); err != nil {
		return fmt.Errorf("reinforce focus: %w", err)
	}

	if _, err := f.q.PruneFocus(ctx, sqlc.PruneFocusParams{
		UserID: userID,
		Cap:    int32(cap),
	}); err != nil {
		return fmt.Errorf("reinforce focus: prune: %w", err)
	}

	return nil
}
