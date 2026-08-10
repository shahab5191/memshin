package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryTag = "short-term"
)

type conversationStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	RecentByUser(ctx context.Context, userID string, limit int) ([]repository.Message, error)
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

	// Promotion is not published yet. It needs the ClaimPromotable /
	// AckPromoted queries

	return nil
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
