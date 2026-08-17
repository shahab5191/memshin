package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/shahab5191/memshin/internal/db/sqlc"
)

// Fact is one atomic proposition extracted from a promoted batch, together with
// the vector it is retrieved by.
type Fact struct {
	Content   string
	Embedding []float32

	// ValidFrom is when the fact became true, which is not when it was heard.
	// Nil when extraction could not tell, and readers fall back to CreatedAt.
	ValidFrom *time.Time

	SourceTurnIDs []uuid.UUID
}

// Probe is one decomposed retrieval intent. Both halves describe the same
// intent: Vector drives dense search, Text drives the lexical half.
type Probe struct {
	Text   string
	Vector []float32
}

type SearchParams struct {
	Probes []Probe

	// MaxDistance discards dense hits no closer than this. Without it a probe
	// always returns its k least-unrelated facts, however unrelated those are.
	MaxDistance float64

	// PerProbe caps each retriever's contribution per probe; MaxResults caps
	// the fused set, which is what actually reaches the prompt.
	PerProbe   int
	MaxResults int
}

// Recollection is a fact retrieved for a turn, carrying enough scoring detail
// to make retrieval quality observable rather than a black box.
type Recollection struct {
	ID        uuid.UUID
	Content   string
	ValidFrom *time.Time
	CreatedAt time.Time

	Score float64

	// BestDistance is the closest dense match across all probes, or 2 — the
	// upper bound of cosine distance — when only the lexical half matched.
	BestDistance float64
}

// EffectiveAt is when the fact became true as best we know it, which is what a
// reader needs to tell a superseded fact from a current one.
func (r Recollection) EffectiveAt() time.Time {
	if r.ValidFrom != nil {
		return *r.ValidFrom
	}
	return r.CreatedAt
}

type Facts struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewFacts(pool *pgxpool.Pool) *Facts {
	return &Facts{pool: pool, q: sqlc.New(pool)}
}

// StoreBatch writes the facts extracted from a release and acknowledges that
// release in the same transaction. The atomicity is the point: a summary that
// exists while its source rows still look unacknowledged would be re-extracted
// on the next doorbell, and rows acknowledged before their facts landed would
// leave a hole no later pass ever fills.
//
// It reports how many messages were settled. Zero means the version was stale —
// the batch was reclaimed and reissued while this caller was working — and is
// not an error; the facts written here are rolled back with it.
func (f *Facts) StoreBatch(
	ctx context.Context,
	userID string,
	version int32,
	turnIDs []uuid.UUID,
	facts []Fact,
) (int64, error) {
	if userID == "" {
		return 0, fmt.Errorf("store batch: empty user id")
	}
	if len(turnIDs) == 0 {
		return 0, fmt.Errorf("store batch: no turns to acknowledge")
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store batch: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := f.q.WithTx(tx)

	// Extraction is not deterministic, so a redelivered batch can yield a
	// different set of facts than the attempt that died before acknowledging.
	// Clearing the release first replaces it wholesale instead of merging two
	// partial extractions into one incoherent set.
	if err := q.DeleteFactsForVersion(ctx, sqlc.DeleteFactsForVersionParams{
		UserID:         userID,
		PublishVersion: version,
	}); err != nil {
		return 0, fmt.Errorf("store batch: clear previous attempt: %w", err)
	}

	for i, fact := range facts {
		id, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("store batch: generate fact id: %w", err)
		}

		var validFrom pgtype.Timestamptz
		if fact.ValidFrom != nil {
			validFrom = pgtype.Timestamptz{Time: *fact.ValidFrom, Valid: true}
		}

		if err := q.InsertFact(ctx, sqlc.InsertFactParams{
			ID:             id,
			UserID:         userID,
			Content:        fact.Content,
			Embedding:      pgvector.NewVector(fact.Embedding),
			SourceTurnIds:  fact.SourceTurnIDs,
			PublishVersion: version,
			FactIndex:      int32(i),
			ValidFrom:      validFrom,
		}); err != nil {
			return 0, fmt.Errorf("store batch: insert fact %d: %w", i, err)
		}
	}

	settled, err := q.MarkPromoted(ctx, sqlc.MarkPromotedParams{
		UserID:         userID,
		TurnIds:        turnIDs,
		PublishVersion: version,
	})
	if err != nil {
		return 0, fmt.Errorf("store batch: mark promoted: %w", err)
	}

	// Nothing settled means the version was fenced out: this batch was
	// reclaimed and reissued while we were extracting, and another worker owns
	// it now. Returning without committing discards our facts along with the
	// acknowledgement — committing them would write facts for a release we lost
	// and let the owner write them again.
	if settled == 0 {
		return 0, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store batch: commit: %w", err)
	}

	return settled, nil
}

// Search runs every probe through both retrievers in a single round trip and
// returns the fused set, most relevant first.
func (f *Facts) Search(ctx context.Context, userID string, p SearchParams) ([]Recollection, error) {
	if userID == "" {
		return nil, fmt.Errorf("search facts: empty user id")
	}
	if len(p.Probes) == 0 {
		return nil, nil
	}
	if p.PerProbe <= 0 || p.MaxResults <= 0 {
		return nil, fmt.Errorf(
			"search facts: per-probe %d and max results %d must both be positive",
			p.PerProbe, p.MaxResults)
	}

	// Serialised to pgvector's text form; the query casts each element back.
	vectors := make([]string, 0, len(p.Probes))
	texts := make([]string, 0, len(p.Probes))
	for i, probe := range p.Probes {
		if len(probe.Vector) == 0 {
			return nil, fmt.Errorf("search facts: probe %d has no vector", i)
		}
		vectors = append(vectors, pgvector.NewVector(probe.Vector).String())
		texts = append(texts, probe.Text)
	}

	rows, err := f.q.SearchFacts(ctx, sqlc.SearchFactsParams{
		UserID:      userID,
		Vectors:     vectors,
		Texts:       texts,
		MaxDistance: p.MaxDistance,
		PerProbe:    int32(p.PerProbe),
		MaxResults:  int32(p.MaxResults),
	})
	if err != nil {
		return nil, fmt.Errorf("search facts: %w", err)
	}

	out := make([]Recollection, 0, len(rows))
	for _, r := range rows {
		rec := Recollection{
			ID:           r.ID,
			Content:      r.Content,
			CreatedAt:    r.CreatedAt,
			Score:        r.Score,
			BestDistance: r.BestDistance,
		}
		if r.ValidFrom.Valid {
			t := r.ValidFrom.Time
			rec.ValidFrom = &t
		}
		out = append(out, rec)
	}

	return out, nil
}
