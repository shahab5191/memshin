package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

// MemoryItem is one retrieved mid-term fact. Similarity is cosine similarity
// (1 - cosine distance), so higher is better; the caller decides the threshold.
type MemoryItem struct {
	Summary    string
	StartSeq   int64
	EndSeq     int64
	Similarity float64
}

// Fact is one atomic, self-contained piece of mid-term memory and its embedding.
type Fact struct {
	Text      string
	Embedding []float32
}

type MidTermMemory struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewMidTermMemory(pool *pgxpool.Pool) *MidTermMemory {
	return &MidTermMemory{pool: pool, q: sqlc.New(pool)}
}

// ReplaceFacts atomically replaces every fact stored for one release, keyed by
// (user_id, publish_version). A replayed or stale write deletes then re-inserts,
// so a different fact count never duplicates or leaves stale rows behind.
func (m *MidTermMemory) ReplaceFacts(
	ctx context.Context,
	userID string,
	facts []Fact,
	startSeq, endSeq int64,
	publishVersion int32,
) error {
	if userID == "" {
		return fmt.Errorf("replace facts: empty user id")
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace facts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := m.q.WithTx(tx)
	if err := q.DeleteFacts(ctx, sqlc.DeleteFactsParams{
		UserID:         userID,
		PublishVersion: publishVersion,
	}); err != nil {
		return fmt.Errorf("replace facts: delete: %w", err)
	}

	for i, fact := range facts {
		if err := q.InsertFact(ctx, sqlc.InsertFactParams{
			UserID:         userID,
			PublishVersion: publishVersion,
			FactIndex:      int32(i),
			Summary:        fact.Text,
			Embedding:      pgvector.NewVector(fact.Embedding),
			StartSeq:       startSeq,
			EndSeq:         endSeq,
		}); err != nil {
			return fmt.Errorf("replace facts: insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace facts: commit: %w", err)
	}

	return nil
}

// Search returns the facts most similar to the query embedding, best first,
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
