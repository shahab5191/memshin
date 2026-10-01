package memory

import (
	"context"

	"github.com/google/uuid"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryName = "ShortTermMemory"
	MidTermMemoryName   = "MidTermMemory"
	LongTermMemoryName  = "LongTermMemory"
	FocusMemoryName     = "FocusMemory"
)

// shortTermStore is what the short-term layer needs: append turns, read its
// window, and release bounded batches downstream.
type shortTermStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
	ReleaseBatch(ctx context.Context, userID, stage string, recentFloor, batchSize int) (int64, error)
}

// promotionStore is what a downstream layer needs to consume released batches:
// claim the oldest outstanding one and acknowledge it once durably stored.
type promotionStore interface {
	ClaimBatch(ctx context.Context, userID, stage string) (repository.Batch, error)
	MarkPromoted(ctx context.Context, userID, stage string, turnIDs []uuid.UUID, version int32) (int64, error)
}

// vectorStore is the mid-term vector backend: write a per-release summary and
// read the summaries most relevant to a query.
type vectorStore interface {
	Upsert(ctx context.Context, userID, summary string, embedding []float32, startSeq, endSeq int64, publishVersion int32) error
	Search(ctx context.Context, userID string, embedding []float32, limit int) ([]repository.MemoryItem, error)
}

type summarizer interface {
	Summarize(ctx context.Context, text string) (string, error)
}

type embedder interface {
	Embed(ctx context.Context, text, taskType string) ([]float32, error)
}
