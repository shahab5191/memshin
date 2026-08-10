package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	ID        uuid.UUID
	TurnID    uuid.UUID
	UserID    string
	Role      Role
	Content   string
	Seq       int64
	CreatedAt time.Time
}

type Conversations struct {
	q *sqlc.Queries
}

func NewConversations(pool *pgxpool.Pool) *Conversations {
	return &Conversations{q: sqlc.New(pool)}
}

func (c *Conversations) AppendTurn(ctx context.Context, userID, prompt, response string) error {
	if userID == "" {
		return fmt.Errorf("append turn: empty user id")
	}

	turnID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("append turn: generate turn id: %w", err)
	}

	rows := make([]sqlc.AppendTurnParams, 0, 2)
	for _, m := range []struct {
		role    Role
		content string
	}{
		{RoleUser, prompt},
		{RoleAssistant, response},
	} {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("append turn: generate message id: %w", err)
		}
		rows = append(rows, sqlc.AppendTurnParams{
			ID:      id,
			UserID:  userID,
			TurnID:  turnID,
			Role:    string(m.role),
			Content: m.content,
		})
	}

	n, err := c.q.AppendTurn(ctx, rows)
	if err != nil {
		return fmt.Errorf("append turn: %w", err)
	}
	if n != int64(len(rows)) {
		return fmt.Errorf("append turn: wrote %d of %d rows", n, len(rows))
	}

	return nil
}

func (c *Conversations) RecentByUser(ctx context.Context, userID string, limit int) ([]Message, error) {
	if userID == "" {
		return nil, fmt.Errorf("recent by user: empty user id")
	}
	// 0 limit means no limit
	arg := sqlc.RecentByUserParams{UserID: userID}
	if limit > 0 {
		arg.LimitCount = pgtype.Int4{Int32: int32(limit), Valid: true}
	}

	rows, err := c.q.RecentByUser(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("recent by user: %w", err)
	}

	messages := make([]Message, 0, len(rows))
	for _, r := range rows {
		messages = append(messages, toMessage(r))
	}

	return messages, nil
}

func toMessage(r sqlc.RecentByUserRow) Message {
	return Message{
		ID:        r.ID,
		TurnID:    r.TurnID,
		UserID:    r.UserID,
		Role:      Role(r.Role),
		Content:   r.Content,
		Seq:       r.Seq,
		CreatedAt: r.CreatedAt,
	}
}
