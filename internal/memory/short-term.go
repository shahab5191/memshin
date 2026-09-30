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
	RecentMessageFloor = 8
	PromotionBatchSize = 8
)

const releaseTimeout = 10 * time.Second

type ShortTermMemory struct {
	store conversationStore
}

func NewShortTermMemory(store conversationStore) *ShortTermMemory {
	return &ShortTermMemory{store: store}
}

func (stm *ShortTermMemory) Name() string {
	return ShortTermMemoryName
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
		Tag:      ShortTermMemoryName,
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

// publishPromotable hands a released batch to mid-term by ringing a doorbell
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
			releaseCtx, userID, MidTermMemoryName, RecentMessageFloor, PromotionBatchSize,
		)
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
