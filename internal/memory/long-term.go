package memory

import (
	"context"

	"github.com/shahab5191/memshin/internal/pipeline"
)

type LongTermMemory struct {
	store conversationStore
}

func NewLongTermMemory(store conversationStore) *LongTermMemory {
	return &LongTermMemory{store: store}
}

func (ltm *LongTermMemory) Name() string {
	return LongTermMemoryName
}

func (ltm *LongTermMemory) RequestProcess(ctx context.Context, chat *pipeline.ChatContext) error {
	return nil
}

func (ltm *LongTermMemory) ResponseProcess(
	ctx context.Context,
	chat *pipeline.ChatContext,
	llmResponse string,
	pub pipeline.Publisher,
) error {
	return nil
}

func (ltm *LongTermMemory) HandlePromotion(
	ctx context.Context,
	event pipeline.PromotionEvent,
	pub pipeline.Publisher,
) error {
	return nil
}
