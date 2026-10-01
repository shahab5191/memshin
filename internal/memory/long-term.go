package memory

import (
	"context"

	"github.com/shahab5191/memshin/internal/promotion"
)

type LongTermMemory struct {
	// Placeholder. Long-term will distill durable facts from mid-term, so it is
	// expected to read the mid-term vector store; its real dependencies are
	// decided in the long-term spec.
	store vectorStore
}

func NewLongTermMemory(store vectorStore) *LongTermMemory {
	return &LongTermMemory{store: store}
}

func (ltm *LongTermMemory) Name() string {
	return LongTermMemoryName
}

func (ltm *LongTermMemory) RequestProcess(ctx context.Context, chat *ChatContext) error {
	return nil
}

func (ltm *LongTermMemory) ResponseProcess(
	ctx context.Context,
	chat *ChatContext,
	llmResponse string,
	pub promotion.Publisher,
) error {
	return nil
}

func (ltm *LongTermMemory) HandlePromotion(
	ctx context.Context,
	event promotion.Event,
	pub promotion.Publisher,
) error {
	return nil
}
