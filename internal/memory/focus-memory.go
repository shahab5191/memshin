package memory

import (
	"context"
	"fmt"

	"github.com/shahab5191/memshin/internal/promotion"
)

type FocusMemory struct {
	stmStore shortTermStore
}

func NewFocusMemory(stmStore shortTermStore) *FocusMemory {
	return &FocusMemory{
		stmStore: stmStore,
	}
}

func (f *FocusMemory) Name() string {
	return FocusMemoryName
}

func (fm *FocusMemory) RequestProcess(ctx context.Context, chat *ChatContext) error {
	return nil
}

func (fm *FocusMemory) ResponseProcess(
	ctx context.Context,
	chat *ChatContext,
	llmResponse string,
	pub promotion.Publisher,
) error {
	return nil
}

func (fm *FocusMemory) HandlePromotion(ctx context.Context, event promotion.Event, pub promotion.Publisher) error {
	latestTurn, err := fm.stmStore.LatestTurn(ctx, event.UserID)
	if err != nil {
		return fmt.Errorf("%s: latest turn: %w", fm.Name(), err)
	}

	if len(latestTurn) == 0 {
		return nil
	}
}
