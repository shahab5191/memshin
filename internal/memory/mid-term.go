package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

// MidTermMemory is the layer that survives the short-term window. It decomposes
// promoted conversation into atomic facts on the way in, and the incoming turn
// into retrieval probes on the way out, so both sides of the vector store speak
// at the same granularity.
type MidTermMemory struct {
	conversations conversationStore
	facts         factStore
	analyst       analyst
	embedder      embedder
}

func NewMidTermMemory(
	conversations conversationStore,
	facts factStore,
	analyst analyst,
	embedder embedder,
) *MidTermMemory {
	return &MidTermMemory{
		conversations: conversations,
		facts:         facts,
		analyst:       analyst,
		embedder:      embedder,
	}
}

func (mtm *MidTermMemory) Name() string {
	return MidTermMemoryName
}

// RequestProcess recalls facts relevant to this turn.
//
// Every failure here is logged and swallowed. Mid-term is an enhancement to a
// turn that can be answered without it, so a decomposer timeout or an embedding
// error degrades recall rather than failing the user's request.
func (mtm *MidTermMemory) RequestProcess(ctx context.Context, chat *pipeline.ChatContext) error {
	probes := mtm.probesFor(ctx, chat)
	if len(probes) == 0 {
		return nil // nothing worth looking up this turn
	}

	vectors, err := mtm.embedder.EmbedQueries(ctx, probes)
	if err != nil {
		slog.Error("mid-term recall skipped: embed probes",
			"layer", mtm.Name(), "user", chat.UserID, "error", err)
		return nil
	}

	search := repository.SearchParams{
		Probes:      make([]repository.Probe, 0, len(probes)),
		MaxDistance: MidTermMaxDistance,
		PerProbe:    MidTermPerProbe,
		MaxResults:  MidTermMaxResults,
	}
	for i, p := range probes {
		search.Probes = append(search.Probes, repository.Probe{Text: p, Vector: vectors[i]})
	}

	recalled, err := mtm.facts.Search(ctx, chat.UserID, search)
	if err != nil {
		slog.Error("mid-term recall skipped: search",
			"layer", mtm.Name(), "user", chat.UserID, "error", err)
		return nil
	}
	if len(recalled) == 0 {
		return nil
	}

	slog.Debug("mid-term recalled",
		"layer", mtm.Name(), "user", chat.UserID,
		"probes", len(probes), "facts", len(recalled),
		"best_distance", recalled[0].BestDistance)

	chat.AddBlock(pipeline.ContextBlock{
		Source: mtm.Name(),
		Tag:    MidTermMemoryTag,
		// Ahead of short-term's 1: recalled history is older than the live
		// thread, and reads as background to it rather than as part of it.
		Priority: 0,
		Content:  renderRecollections(recalled),
	})

	return nil
}

// probesFor decomposes the turn into retrieval statements, falling back to the
// raw prompt if the decomposer is unavailable.
//
// The fallback is deliberately not "retrieve nothing". A failed decomposition
// and a decomposition that legitimately returns no probes are different
// answers, and only the second one means this turn needs no memory.
func (mtm *MidTermMemory) probesFor(ctx context.Context, chat *pipeline.ChatContext) []string {
	// Empty until the focus layer runs ahead of this one, in which case the
	// decomposer works from the prompt alone and unresolved references simply
	// produce weaker probes.
	probes, err := mtm.analyst.DecomposeQuery(ctx, chat.OriginalPrompt, chat.Focus)
	if err != nil {
		slog.Warn("query decomposition failed, probing with raw prompt",
			"layer", mtm.Name(), "user", chat.UserID, "error", err)
		return []string{chat.OriginalPrompt}
	}

	if len(probes) == 0 {
		slog.Debug("mid-term skipped by decomposer",
			"layer", mtm.Name(), "user", chat.UserID, "prompt", chat.OriginalPrompt)
	}

	return probes
}

// ResponseProcess does nothing: short-term owns appending the turn, and
// mid-term only ever ingests through the promotion doorbell.
func (mtm *MidTermMemory) ResponseProcess(
	_ context.Context,
	_ *pipeline.ChatContext,
	_ string,
	_ pipeline.Publisher,
) error {
	return nil
}

// HandlePromotion ingests a released batch: read what is outstanding, decompose
// it into facts, embed them, and store them acknowledged in one transaction.
//
// The doorbell carries no content, so a redelivered event re-reads current state
// and finds whatever is still outstanding — which, for a batch already settled,
// is nothing.
func (mtm *MidTermMemory) HandlePromotion(
	ctx context.Context,
	event pipeline.PromotionEvent,
	_ pipeline.Publisher,
) error {
	batch, err := mtm.conversations.PublishedBatch(ctx, event.UserID)
	if err != nil {
		return fmt.Errorf("%s: read published batch: %w", mtm.Name(), err)
	}
	if len(batch.Messages) == 0 {
		return nil // already settled by an earlier delivery
	}

	facts, err := mtm.analyst.ExtractFacts(ctx, renderMessages(batch.Messages))
	if err != nil {
		return fmt.Errorf("%s: extract facts: %w", mtm.Name(), err)
	}

	contents := make([]string, 0, len(facts))
	for _, f := range facts {
		contents = append(contents, f.Content)
	}

	// Not an early return. A stretch of conversation holding nothing durable is
	// a normal outcome, and it still has to be acknowledged: leaving it
	// outstanding stops every later release for this user and grows the
	// short-term window without bound.
	var embeddings [][]float32
	if len(contents) > 0 {
		embeddings, err = mtm.embedder.EmbedDocuments(ctx, contents)
		if err != nil {
			return fmt.Errorf("%s: embed facts: %w", mtm.Name(), err)
		}
	}

	turnIDs := batch.TurnIDs()
	stored := make([]repository.Fact, 0, len(facts))
	for i, f := range facts {
		stored = append(stored, repository.Fact{
			Content:   f.Content,
			Embedding: embeddings[i],
			ValidFrom: f.ValidFrom,
			// Attributed to the whole release: extraction reports what it found,
			// not which turn it came from, so claiming finer provenance than we
			// have would be a lie the consolidation pass later relies on.
			SourceTurnIDs: turnIDs,
		})
	}

	settled, err := mtm.facts.StoreBatch(ctx, event.UserID, batch.Version, turnIDs, stored)
	if err != nil {
		return fmt.Errorf("%s: store batch: %w", mtm.Name(), err)
	}
	if settled == 0 {
		// The version was stale: this batch was reclaimed and reissued while we
		// were summarising, and another worker owns it now. Our facts rolled
		// back with the acknowledgement.
		slog.Warn("mid-term promotion superseded",
			"layer", mtm.Name(), "user", event.UserID, "version", batch.Version)
		return nil
	}

	slog.Info("mid-term ingested batch",
		"layer", mtm.Name(), "user", event.UserID,
		"messages", len(batch.Messages), "facts", len(stored), "settled", settled)

	return nil
}

// renderRecollections stamps each fact with when it became true. Two facts that
// contradict each other both retrieve, and the timestamps are what let the
// model tell the superseded one from the current one.
func renderRecollections(recalled []repository.Recollection) string {
	var b strings.Builder
	for i, r := range recalled {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s] %s", r.EffectiveAt().Format("2006-01-02"), r.Content)
	}
	return b.String()
}
