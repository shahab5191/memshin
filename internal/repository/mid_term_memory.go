package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

// MemoryItem is one retrieved mid-term summary. Similarity is cosine similarity
// (1 - cosine distance), so higher is better; the caller decides the threshold.
type MemoryItem struct {
	Summary    string
	StartSeq   int64
	EndSeq     int64
	Similarity float64
}

type MidTermMemory struct {
	q *sqlc.Queries
}

func NewMidTermMemory(pool *pgxpool.Pool) *MidTermMemory {
	return &MidTermMemory{q: sqlc.New(pool)}
}

// Upsert writes (or overwrites) the summary for one release, keyed by
// (user_id, publish_version). A replayed or stale write updates in place, so
// completion order never duplicates a batch.
func (m *MidTermMemory) Upsert(
	ctx context.Context,
	userID, summary string,
	embedding []float32,
	startSeq, endSeq int64,
	publishVersion int32,
) error {
	if userID == "" {
		return fmt.Errorf("upsert memory: empty user id")
	}

	err := m.q.UpsertMemory(ctx, sqlc.UpsertMemoryParams{
		UserID:         userID,
		PublishVersion: publishVersion,
		Summary:        summary,
		Embedding:      pgvector.NewVector(embedding),
		StartSeq:       startSeq,
		EndSeq:         endSeq,
	})
	if err != nil {
		return fmt.Errorf("upsert memory: %w", err)
	}

	return nil
}

// Search returns the summaries most similar to the query embedding, best first,
// scoped to the requesting user. The caller applies the minimum-similarity
// threshold.
func (m *MidTermMemory) Search(
	ctx context.Context,
	userID string,
	embedding []float32,
	limit int,
) ([]MemoryItem, error) {
	if userID == "" {
		return nil, fmt.Errorf("search memory: empty user id")
	}

	rows, err := m.q.SearchMemory(ctx, sqlc.SearchMemoryParams{
		UserID:    userID,
		Embedding: pgvector.NewVector(embedding),
		LimitRows: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("search memory: %w", err)
	}

	items := make([]MemoryItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, MemoryItem{
			Summary:    r.Summary,
			StartSeq:   r.StartSeq,
			EndSeq:     r.EndSeq,
			Similarity: r.Similarity,
		})
	}

	return items, nil
}
