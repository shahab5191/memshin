// Package app wires the memory pipeline together. It is shared by cmd/api (which
// runs the layers behind an HTTP server) and cmd/memtest (which drives them
// synchronously against an ephemeral database), so the two cannot drift apart.
package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shahab5191/memshin/internal/memory"
	"github.com/shahab5191/memshin/internal/repository"
)

// Provider is the subset of the LLM client the memory layers need: focus
// extraction, mid-term summarisation and embedding. *llm.Gemini satisfies it;
// the interface lets tests substitute a fake.
type Provider interface {
	Extract(ctx context.Context, currentSubject, text string) (keep bool, subject string, err error)
	Summarize(ctx context.Context, text string) ([]string, error)
	Embed(ctx context.Context, text, taskType string) ([]float32, error)
}

// MemoryPipeline holds the assembled memory layers plus the concrete handles a
// caller may need. Layers is ordered for the pipeline engine; the other fields
// expose the same instances for direct, synchronous orchestration.
type MemoryPipeline struct {
	Layers    []memory.MemoryLayer
	ShortTerm *memory.ShortTermMemory
	Focus     *memory.FocusMemory
	MidTerm   *memory.MidTermMemory

	// Conversations is the short-term store shared by the layers; the harness
	// uses it directly to append turns and release batches.
	Conversations *repository.Conversations
}

// NewMemoryPipeline builds the memory layers in the exact order cmd/api uses:
// short-term first, then focus (before mid-term so FocusSubject is set for
// retrieval), then mid-term (which consumes what short-term releases).
func NewMemoryPipeline(pool *pgxpool.Pool, provider Provider) *MemoryPipeline {
	conv := repository.NewConversations(pool)
	focus := repository.NewFocus(pool)
	vector := repository.NewMidTermMemory(pool)

	short := memory.NewShortTermMemory(conv)
	fm := memory.NewFocusMemory(conv, focus, provider)
	mtm := memory.NewMidTermMemory(conv, vector, provider, provider)

	return &MemoryPipeline{
		Layers:        []memory.MemoryLayer{short, fm, mtm},
		ShortTerm:     short,
		Focus:         fm,
		MidTerm:       mtm,
		Conversations: conv,
	}
}
