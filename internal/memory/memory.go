package memory

import (
	"context"

	"github.com/google/uuid"

	"github.com/shahab5191/memshin/internal/llm"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	ShortTermMemoryTag = "ShortTermMemory"
	MidTermMemoryName  = "MidTermMemory"
	MidTermMemoryTag   = "MidTermMemory"
	RecentMessageFloor = 8  // how many recent messages to keep in short-term memory
	PromotionThreshold = 16 // how much backlog is enough to trigger a promotion to mid-term memory
)

// Retrieval limits for mid-term. These are starting points, not settled values:
// the distance ceiling in particular has to be tuned against real recalls,
// because it is the only thing standing between a question mid-term knows
// nothing about and a plausible irrelevant fact being stated back as truth.
const (
	MidTermMaxDistance = 0.6
	MidTermPerProbe    = 5
	MidTermMaxResults  = 8
)

type conversationStore interface {
	AppendTurn(ctx context.Context, userID, prompt, response string) error
	ShortTermWindow(ctx context.Context, userID string, recentCount int) ([]repository.Message, error)
	ClaimPromotable(ctx context.Context, userID string, threshold, recentFloor int) (int64, error)
	PublishedBatch(ctx context.Context, userID string) (repository.PublishedBatch, error)
}

// factStore is the vector half of mid-term. Search and StoreBatch are the whole
// surface: everything else about how facts are ranked lives in SQL.
type factStore interface {
	StoreBatch(ctx context.Context, userID string, version int32,
		turnIDs []uuid.UUID, facts []repository.Fact) (int64, error)
	Search(ctx context.Context, userID string, p repository.SearchParams) ([]repository.Recollection, error)
}

type embedder interface {
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQueries(ctx context.Context, texts []string) ([][]float32, error)
}

// analyst decomposes in both directions: a transcript into storable facts, and
// a turn into retrieval probes.
type analyst interface {
	ExtractFacts(ctx context.Context, transcript string) ([]llm.ExtractedFact, error)
	DecomposeQuery(ctx context.Context, prompt string, focus []string) ([]string, error)
}
