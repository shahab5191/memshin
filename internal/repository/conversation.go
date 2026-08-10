package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
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

// ShortTermWindow returns every message not yet promoted into mid-term, plus
// the turns covering the newest recentCount messages even if those were already
// promoted. recentCount is a floor, not a cap: the result is the union of the
// two, so it is never shorter than the recent window.
func (c *Conversations) ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]Message, error) {
	if userID == "" {
		return nil, fmt.Errorf("short term window: empty user id")
	}
	if recentCount < 0 {
		return nil, fmt.Errorf("short term window: negative recent count %d", recentCount)
	}

	rows, err := c.q.ShortTermWindow(ctx, sqlc.ShortTermWindowParams{
		UserID:      userID,
		RecentCount: int32(recentCount),
	})
	if err != nil {
		return nil, fmt.Errorf("short term window: %w", err)
	}

	messages := make([]Message, 0, len(rows))
	for _, r := range rows {
		messages = append(messages, toMessage(r))
	}

	return messages, nil
}

// PublishedBatch is a set of messages released to a downstream layer and not
// yet acknowledged, together with the version they must be acknowledged under.
type PublishedBatch struct {
	Messages []Message
	Version  int32
}

// TurnIDs returns the distinct turns in the batch, in order, which is the unit
// MarkPromoted acknowledges in.
func (b PublishedBatch) TurnIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(b.Messages))
	var last uuid.UUID
	for _, m := range b.Messages {
		if m.TurnID != last {
			ids = append(ids, m.TurnID)
			last = m.TurnID
		}
	}
	return ids
}

// ClaimPromotable releases everything above recentFloor to the next layer, but
// only once the backlog has reached threshold and no earlier release is still
// unacknowledged. It returns how many messages it let go, which is zero on
// every turn that does not trip the gate.
func (c *Conversations) ClaimPromotable(ctx context.Context, userID string, threshold, recentFloor int) (int64, error) {
	if userID == "" {
		return 0, fmt.Errorf("claim promotable: empty user id")
	}
	if recentFloor < 0 {
		return 0, fmt.Errorf("claim promotable: negative recent floor %d", recentFloor)
	}
	// The query releases (backlog - recentFloor) rows once the backlog reaches
	// threshold. Were threshold the smaller of the two, that count would go
	// negative and Postgres would reject the LIMIT outright.
	if threshold <= recentFloor {
		return 0, fmt.Errorf(
			"claim promotable: threshold %d must exceed recent floor %d", threshold, recentFloor)
	}

	released, err := c.q.ClaimPromotable(ctx, sqlc.ClaimPromotableParams{
		UserID:      userID,
		Threshold:   int64(threshold),
		RecentFloor: int64(recentFloor),
	})
	if err != nil {
		return 0, fmt.Errorf("claim promotable: %w", err)
	}

	return released, nil
}

// PublishedBatch returns the messages released to a downstream layer and still
// awaiting acknowledgement. It is empty when nothing is outstanding.
func (c *Conversations) PublishedBatch(ctx context.Context, userID string) (PublishedBatch, error) {
	if userID == "" {
		return PublishedBatch{}, fmt.Errorf("published batch: empty user id")
	}

	rows, err := c.q.PublishedBatch(ctx, userID)
	if err != nil {
		return PublishedBatch{}, fmt.Errorf("published batch: %w", err)
	}
	if len(rows) == 0 {
		return PublishedBatch{}, nil
	}

	batch := PublishedBatch{
		Messages: make([]Message, 0, len(rows)),
		Version:  rows[0].PublishVersion,
	}
	for _, r := range rows {
		batch.Messages = append(batch.Messages, Message{
			ID:        r.ID,
			TurnID:    r.TurnID,
			UserID:    r.UserID,
			Role:      Role(r.Role),
			Content:   r.Content,
			Seq:       r.Seq,
			CreatedAt: r.CreatedAt,
		})
		// A release is stamped with one version, so a mismatch means two
		// releases are outstanding at once — which the claim gate is supposed
		// to make impossible. Fail loudly rather than acknowledge half a batch.
		if r.PublishVersion != batch.Version {
			return PublishedBatch{}, fmt.Errorf(
				"published batch: mixed publish versions %d and %d for user %s",
				batch.Version, r.PublishVersion, userID)
		}
	}

	return batch, nil
}

// MarkPromoted acknowledges turns the downstream layer has durably stored, so
// they leave the short-term window. It reports how many messages it settled;
// zero means the version was stale — the batch was reclaimed and reissued
// while this caller was working — and is not an error.
func (c *Conversations) MarkPromoted(
	ctx context.Context,
	userID string,
	turnIDs []uuid.UUID,
	version int32,
) (int64, error) {
	if userID == "" {
		return 0, fmt.Errorf("mark promoted: empty user id")
	}
	if len(turnIDs) == 0 {
		return 0, nil
	}

	n, err := c.q.MarkPromoted(ctx, sqlc.MarkPromotedParams{
		UserID:         userID,
		TurnIds:        turnIDs,
		PublishVersion: version,
	})
	if err != nil {
		return 0, fmt.Errorf("mark promoted: %w", err)
	}

	return n, nil
}

func toMessage(r sqlc.ShortTermWindowRow) Message {
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
