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

// Batch is a set of messages a downstream layer has claimed for summarisation,
// together with the version they must be acknowledged under.
type Batch struct {
	Messages []Message
	Version  int32
}

// TurnIDs returns the distinct turns in the batch, in order, which is the unit
// MarkPromoted acknowledges in.
func (b Batch) TurnIDs() []uuid.UUID {
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

// ReleaseBatch releases a bounded, versioned batch of backlog to the given
// stage, but only once a full batch has accumulated above the floor. It returns
// how many messages it let go, which is zero on every turn without a full batch
// available. Releases never wait for an earlier release to be acknowledged:
// batches are versioned and consumed oldest-first, so several may be in flight.
func (c *Conversations) ReleaseBatch(
	ctx context.Context,
	userID, stage string,
	recentFloor, batchSize int,
) (int64, error) {
	if userID == "" {
		return 0, fmt.Errorf("release batch: empty user id")
	}
	if stage == "" {
		return 0, fmt.Errorf("release batch: empty stage")
	}
	if recentFloor < 0 {
		return 0, fmt.Errorf("release batch: negative recent floor %d", recentFloor)
	}
	if batchSize <= 0 {
		return 0, fmt.Errorf("release batch: non-positive batch size %d", batchSize)
	}

	released, err := c.q.ReleaseBatch(ctx, sqlc.ReleaseBatchParams{
		UserID:      userID,
		RecentFloor: int64(recentFloor),
		BatchSize:   int64(batchSize),
		Stage:       stage,
	})
	if err != nil {
		return 0, fmt.Errorf("release batch: %w", err)
	}

	return released, nil
}

// ClaimBatch atomically claims the oldest unclaimed batch released to the given
// stage, transitioning it to 'processing' under the row lock. It is empty
// (Version 0, no messages) when there is nothing to summarise; a worker that
// loses the race to another gets exactly that empty result and exits without a
// summarisation.
func (c *Conversations) ClaimBatch(ctx context.Context, userID, stage string) (Batch, error) {
	if userID == "" {
		return Batch{}, fmt.Errorf("claim batch: empty user id")
	}

	rows, err := c.q.ClaimBatch(ctx, sqlc.ClaimBatchParams{UserID: userID, Stage: stage})
	if err != nil {
		return Batch{}, fmt.Errorf("claim batch: %w", err)
	}
	if len(rows) == 0 {
		return Batch{}, nil
	}

	batch := Batch{
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
		// A claim is stamped with one version, so a mismatch means the claim
		// spans two releases — which claiming by min(version) makes impossible.
		// Fail loudly rather than acknowledge half a batch.
		if r.PublishVersion != batch.Version {
			return Batch{}, fmt.Errorf(
				"claim batch: mixed publish versions %d and %d for user %s",
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
	userID, stage string,
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
		Stage:          stage,
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

func (c *Conversations) LatestTurn(ctx context.Context, userID string) ([]Message, error) {
	if userID == "" {
		return []Message{}, fmt.Errorf("latest turn: empty user id")
	}
	rows, err := c.q.LatestTurn(ctx, userID)
	if err != nil {
		return []Message{}, fmt.Errorf("claim batch: %w", err)
	}
	if len(rows) == 0 {
		return []Message{}, nil
	}

	messages := []Message{}
	for _, r := range rows {
		messages = append(messages, Message{
			ID:        r.ID,
			TurnID:    r.TurnID,
			UserID:    r.UserID,
			Role:      Role(r.Role),
			Content:   r.Content,
			Seq:       r.Seq,
			CreatedAt: r.CreatedAt,
		})
	}

	return messages, nil
}
