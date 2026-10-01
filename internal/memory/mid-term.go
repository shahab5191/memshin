package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/shahab5191/memshin/internal/promotion"
	"github.com/shahab5191/memshin/internal/repository"
)

const (
	// midTermTopK bounds how many summaries a request injects. midTermMinSimilarity
	// drops results below that cosine similarity. Constants first; promoted to
	// environment variables later if needed.
	midTermTopK          = 5
	midTermMinSimilarity = 0.3
)

// Embedding task types. These are the retrieval model's task strings: the store
// side and the search side must agree for the model to compare them sensibly.
const (
	retrievalDocumentTaskType = "RETRIEVAL_DOCUMENT"
	retrievalQueryTaskType    = "RETRIEVAL_QUERY"
)

type MidTermMemory struct {
	store     promotionStore
	vector    vectorStore
	summarize summarizer
	embed     embedder
}

func NewMidTermMemory(store promotionStore, vector vectorStore, summarize summarizer, embed embedder) *MidTermMemory {
	return &MidTermMemory{
		store:     store,
		vector:    vector,
		summarize: summarize,
		embed:     embed,
	}
}

func (mtm *MidTermMemory) Name() string {
	return MidTermMemoryName
}

// RequestProcess retrieves the summaries relevant to the current prompt and
// injects them below short-term (Priority 2 vs short-term's 1).
func (mtm *MidTermMemory) RequestProcess(ctx context.Context, chat *ChatContext) error {
	if chat.OriginalPrompt == "" {
		return nil
	}

	embedding, err := mtm.embed.Embed(ctx, chat.OriginalPrompt, retrievalQueryTaskType)
	if err != nil {
		return fmt.Errorf("%s: embed prompt: %w", mtm.Name(), err)
	}

	items, err := mtm.vector.Search(ctx, chat.UserID, embedding, midTermTopK)
	if err != nil {
		return fmt.Errorf("%s: search memory: %w", mtm.Name(), err)
	}

	// Results are best-first, so the first below the threshold means the rest
	// are too.
	var kept []repository.MemoryItem
	for _, item := range items {
		if item.Similarity < midTermMinSimilarity {
			break
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		return nil
	}

	chat.AddBlock(ContextBlock{
		Source:   mtm.Name(),
		Tag:      MidTermMemoryName,
		Content:  renderSummaries(kept),
		Priority: 2,
	})

	return nil
}

// ResponseProcess is a no-op: mid-term is fed by promotion, not per-turn.
func (mtm *MidTermMemory) ResponseProcess(
	ctx context.Context,
	chat *ChatContext,
	llmResponse string,
	pub promotion.Publisher,
) error {
	return nil
}

// HandlePromotion drains to quiescence: it claims and stores every outstanding
// batch for the user, not just the oldest, so a backlog of released batches
// clears from a single doorbell.
func (mtm *MidTermMemory) HandlePromotion(ctx context.Context, event promotion.Event, pub promotion.Publisher) error {
	for {
		batch, err := mtm.store.ClaimBatch(ctx, event.UserID, MidTermMemoryName)
		if err != nil {
			return fmt.Errorf("%s: claim batch: %w", mtm.Name(), err)
		}
		if len(batch.Messages) == 0 {
			return nil // nothing left to summarise
		}

		if err := mtm.summarizeAndStore(ctx, event.UserID, batch); err != nil {
			return err
		}

		if _, err := mtm.store.MarkPromoted(ctx, event.UserID, MidTermMemoryName,
			batch.TurnIDs(), batch.Version); err != nil {
			return fmt.Errorf("%s: mark promoted: %w", mtm.Name(), err)
		}
	}
}

func (mtm *MidTermMemory) summarizeAndStore(ctx context.Context, userID string, batch repository.Batch) error {
	summary, err := mtm.summarize.Summarize(ctx, renderMessages(batch.Messages))
	if err != nil {
		return fmt.Errorf("%s: summarize batch: %w", mtm.Name(), err)
	}

	embedding, err := mtm.embed.Embed(ctx, summary, retrievalDocumentTaskType)
	if err != nil {
		return fmt.Errorf("%s: embed summary: %w", mtm.Name(), err)
	}

	// Messages arrive seq-ordered, so the first and last elements are the
	// lowest and highest seq in the batch.
	first := batch.Messages[0].Seq
	last := batch.Messages[len(batch.Messages)-1].Seq

	if err := mtm.vector.Upsert(ctx, userID, summary, embedding, first, last, batch.Version); err != nil {
		return fmt.Errorf("%s: store summary: %w", mtm.Name(), err)
	}

	return nil
}

// renderSummaries formats each retrieved summary with the [start_seq, end_seq]
// range of raw messages it covers, so the AI can reference the raw chat later.
func renderSummaries(items []repository.MemoryItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(item.Summary)
		fmt.Fprintf(&b, " [%d-%d]\n", item.StartSeq, item.EndSeq)
	}
	return strings.TrimRight(b.String(), "\n")
}
