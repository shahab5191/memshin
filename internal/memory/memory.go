package memory

import (
	"context"

	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryName = "ShortTermMemory"
	MidTermMemoryName  = "MidTermMemory"
	LongTermMemoryName = "LongTermMemory"
	FocusMemoryName    = "FocusMemory"
)

type conversationStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
	ReleaseBatch(ctx context.Context, userID, stage string, recentFloor, batchSize int) (int64, error)
}
