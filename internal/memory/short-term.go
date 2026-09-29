package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryTag = "ShortTermMemory"
	MidTermMemoryName  = "MidTermMemory"

	// RecentMessageFloor is the tail that stays in short term no matter what:
	// the last four exchanges, two messages each. Mid-term promoting a message
	// removes it from the first half of the window, so without this floor the
	// immediate thread would vanish from under the model the moment it was
	// summarised elsewhere.
	RecentMessageFloor = 8

	// PromotionBatchSize is how many messages each release hands to mid-term.
	// A release fires only once a full batch has accumulated above the floor,
	// so mid-term summarises a fixed-size stretch (four exchanges) rather than
	// a couple of messages per turn, and never an unbounded backlog at once.
	PromotionBatchSize = 8
)

// promotionThreshold is the backlog size at which a full batch is available
// above the floor: a release fires when at least batchSize messages are queued
// beyond the floor, and hands over exactly batchSize of them.
const promotionThreshold = RecentMessageFloor + PromotionBatchSize

// releaseTimeout bounds the detached release work so a slow or stalled database
// cannot leak goroutines forever.
const releaseTimeout = 10 * time.Second

type conversationStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
	ReleaseBatch(ctx context.Context, userID, stage string, threshold, batchSize int) (int64, error)
}

type ShortTermMemory struct {
	store conversationStore
}

func NewShortTermMemory(store conversationStore) *ShortTermMemory {
	return &ShortTermMemory{store: store}
}

func (stm *ShortTermMemory) Name() string {
	return "ShortTermMemory"
}

func (stm *ShortTermMemory) RequestProcess(ctx context.Context, chat *pipeline.ChatContext) error {
	messages, err := stm.store.ShortTermWindow(ctx, chat.UserID, RecentMessageFloor)
	if err != nil {
		return fmt.Errorf("%s: load conversation: %w", stm.Name(), err)
	}
	if len(messages) == 0 {
		return nil // first turn — nothing to inject
	}

	chat.AddBlock(pipeline.ContextBlock{
		Source:   stm.Name(),
		Tag:      ShortTermMemoryTag,
		Content:  renderMessages(messages),
		Priority: 1,
	})

	return nil
}

func (stm *ShortTermMemory) ResponseProcess(
	ctx context.Context,
	chat *pipeline.ChatContext,
	llmResponse string,
	pub pipeline.Publisher,
) error {
	if err := stm.store.AppendTurn(ctx, chat.UserID, chat.OriginalPrompt, llmResponse); err != nil {
		return fmt.Errorf("%s: append turn: %w", stm.Name(), err)
	}

	stm.publishPromotable(ctx, chat.UserID, pub)

	return nil
}

// publishPromotable hands a released batch to mid-term by ringing a doorbell:
// the event says only whose turn it is, and mid-term reads the rows it has been
// given from the store. It runs on a detached context in its own goroutine, so
// the response path never waits on the release and the request's cancellation
// cannot abort it. Failures here are logged, never returned — the turn itself
// is already durable, and the release stands in the database whether or not the
// notification lands.
func (stm *ShortTermMemory) publishPromotable(
	ctx context.Context,
	userID string,
	pub pipeline.Publisher,
) {
	if pub == nil {
		return // no dispatcher wired
	}

	go func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()

		released, err := stm.store.ReleaseBatch(
			releaseCtx, userID, repository.StageMidTerm, promotionThreshold, PromotionBatchSize)
		if err != nil {
			slog.Error("release batch failed", "layer", stm.Name(), "user", userID, "error", err)
			return
		}
		if released == 0 {
			return // no full batch available above the floor yet
		}

		event := pipeline.PromotionEvent{
			UserID:      userID,
			SourceLayer: stm.Name(),
			TargetLayer: MidTermMemoryName,
		}

		if err := pub.Publish(releaseCtx, event); err != nil {
			slog.Warn("promotion not published",
				"layer", stm.Name(), "user", userID, "messages", released, "error", err)
		}
	}()
}

func (stm *ShortTermMemory) HandlePromotion(
	ctx context.Context,
	event pipeline.PromotionEvent,
	pub pipeline.Publisher,
) error {
	return nil
}

func renderMessages(messages []repository.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
