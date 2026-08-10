package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryTag = "short-term"

	// MidTermMemoryName is the routing key for published promotions. The
	// dispatcher matches it against MemoryLayer.Name(), so until a layer
	// answering to this name is registered, every event logs an
	// unknown-layer warning — visible by design rather than silently dropped.
	MidTermMemoryName = "MidTermMemory"
)

type conversationStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	RecentByUser(ctx context.Context, userID string, limit int) ([]repository.Message, error)
	ClaimPromotable(ctx context.Context, userID string) ([]repository.Message, error)
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
	messages, err := stm.store.RecentByUser(ctx, chat.UserID, 0)
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
	promCh chan<- pipeline.PromotionEvent,
) error {
	if err := stm.store.AppendTurn(ctx, chat.UserID, chat.OriginalPrompt, llmResponse); err != nil {
		return fmt.Errorf("%s: append turn: %w", stm.Name(), err)
	}

	stm.publishPromotable(ctx, chat.UserID, promCh)

	return nil
}

func (stm *ShortTermMemory) publishPromotable(
	ctx context.Context,
	userID string,
	promCh chan<- pipeline.PromotionEvent,
) {
	if promCh == nil {
		return // no dispatcher wired; a send would block forever
	}

	messages, err := stm.store.ClaimPromotable(ctx, userID)
	if err != nil {
		slog.Error("claim promotable failed", "layer", stm.Name(), "user", userID, "error", err)
		return
	}
	if len(messages) == 0 {
		return
	}

	blocks := make([]pipeline.ContextBlock, 0, len(messages))
	for _, m := range messages {
		blocks = append(blocks, pipeline.ContextBlock{
			Source:   stm.Name(),
			Tag:      ShortTermMemoryTag,
			Content:  string(m.Role) + ": " + m.Content,
			Priority: 1,
		})
	}

	event := pipeline.PromotionEvent{
		UserID:      userID,
		TargetLayer: MidTermMemoryName,
		Payload: pipeline.PromotionPayload{
			SourceLayer: ShortTermMemoryTag,
			Content:     blocks,
		},
	}

	select {
	case promCh <- event:
	default:
		slog.Warn("promotion queue full, dropping event",
			"layer", stm.Name(), "user", userID, "messages", len(messages))
	}
}

func (stm *ShortTermMemory) HandlePromotion(
	ctx context.Context,
	userID string,
	payload pipeline.PromotionPayload,
	promCh chan<- pipeline.PromotionEvent,
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
