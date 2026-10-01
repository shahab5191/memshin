package memory

import (
	"context"

	"github.com/google/uuid"
	"github.com/shahab5191/memshin/internal/promotion"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryName = "ShortTermMemory"
	MidTermMemoryName   = "MidTermMemory"
	LongTermMemoryName  = "LongTermMemory"
	FocusMemoryName     = "FocusMemory"
)

// MemoryLayer is the contract every memory stage implements. The pipeline
// engine iterates over a slice of these: assembling context before generation,
// recording the turn after, and handling promotion doorbells dispatched by name.
type MemoryLayer interface {
	Name() string
	RequestProcess(ctx context.Context, chat *ChatContext) error
	ResponseProcess(ctx context.Context, chat *ChatContext, llmResponse string, pub promotion.Publisher) error
	HandlePromotion(ctx context.Context, event promotion.Event, pub promotion.Publisher) error
}

// shortTermStore is what the short-term layer needs: append turns, read its
// window, and release bounded batches downstream.
type shortTermStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
	ReleaseBatch(ctx context.Context, userID, stage string, recentFloor, batchSize int) (int64, error)
}

// focusSource is what the focus layer reads to extract the current subject: the
// full short-term window, both sides of every exchange in it.
type focusSource interface {
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
}

// focusStore is the focus layer's append-only subject history.
type focusStore interface {
	GetFocus(ctx context.Context, userID string) (string, error)
	InsertFocus(ctx context.Context, userID, subject string, lastTurnSeq int64) error
}

// extractor distils the current one-sentence subject from the short-term window,
// deciding whether to keep the existing subject or replace it.
type extractor interface {
	Extract(ctx context.Context, currentSubject, text string) (keep bool, subject string, err error)
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
