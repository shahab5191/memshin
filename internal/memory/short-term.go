package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

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

func (stm *ShortTermMemory) publishPromotable(
	ctx context.Context,
	userID string,
	pub pipeline.Publisher,
) {
	if pub == nil {
		return // no dispatcher wired
	}

	released, err := stm.store.ClaimPromotable(ctx, userID, PromotionThreshold, RecentMessageFloor)
	if err != nil {
		slog.Error("claim promotable failed", "layer", stm.Name(), "user", userID, "error", err)
		return
	}
	if released == 0 {
		return // backlog still under the threshold, or an earlier release is outstanding
	}

	event := pipeline.PromotionEvent{
		UserID:      userID,
		SourceLayer: stm.Name(),
		TargetLayer: MidTermMemoryName,
	}

	if err := pub.Publish(ctx, event); err != nil {
		slog.Warn("promotion not published",
			"layer", stm.Name(), "user", userID, "messages", released, "error", err)
	}
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
