# Mid-Term Memory (Vector Store) — Spec

Status: **spec** — not yet implemented.

This document specifies the mid-term memory layer: it consumes the bounded
batches short-term releases, summarizes each batch with the LLM, embeds the
summary with Gemini `text-embedding-004`, stores it in a `pgvector` table in the
existing Postgres, and retrieves the summaries relevant to a request — always
scoped to the requesting user.

Long-term memory is explicitly **out of scope** here. It will be a separate
layer, added later, that distills unique durable facts during periods of
inactivity.

---

## 1. Context and decisions

### 1.1 Naming

The per-chunk summaries live in the **mid-term** layer. The table is
`mid_term_memory` and the layer is `MidTermMemory` (`MidTermMemoryName =
"MidTermMemory"`). The existing `long-term.go` skeleton is untouched; long-term
is specified in a future document.

### 1.2 Locked decisions

| Decision | Value |
|---|---|
| Vector store | `pgvector` extension in the existing Postgres |
| Embedding model | Gemini `text-embedding-004`, 768 dimensions |
| Summary granularity | one summary per batch |
| Idempotency key | `(user_id, publish_version)` |
| Chat-range reference | `start_seq` / `end_seq` (bigint, `conversation.seq`) |
| Retrieval scope | always filtered by `user_id` |
| Distance metric | cosine (`<=>` / `vector_cosine_ops`) |
| Embedding task types | `RETRIEVAL_DOCUMENT` (store), `RETRIEVAL_QUERY` (search) |

### 1.3 Why `start_seq` / `end_seq` survive a stateless system

Each summary row records the range of raw messages it summarizes
(`conversation.seq`). When the layer injects a retrieved summary into a request,
it includes that range. The AI therefore has the reference it needs **at the
moment it decides** whether to fetch the raw conversation — even though the
system is stateless and the AI may decide "now" or "later". Without the range
carried on the summary, a later decision would have nothing to query. The actual
tool that serves the raw conversation is specified separately; this document
only guarantees the range is persisted and surfaced.

---

## 2. Schema

Migration `00004_create_mid_term_memory.sql`.

```sql
-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE mid_term_memory (
    user_id         text        NOT NULL,
    publish_version int         NOT NULL,
    summary         text        NOT NULL,
    embedding       vector(768) NOT NULL,
    start_seq       bigint      NOT NULL,
    end_seq         bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, publish_version)
);

CREATE INDEX mid_term_memory_user_idx ON mid_term_memory (user_id);

CREATE INDEX mid_term_memory_embedding_idx ON mid_term_memory
    USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE mid_term_memory;
DROP EXTENSION vector;
```

Notes:

- The composite primary key `(user_id, publish_version)` makes the upsert
  natural and guarantees one summary row per release per user.
- `embedding` dimension `768` is fixed to `text-embedding-004`. If the model
  changes, the migration changes with it.
- The HNSW index is approximate; for small data it may be skipped or replaced
  by an exact scan. The per-user `user_id` filter is the real access path for
  the common case.

---

## 3. Embedding

Add to the Gemini provider in `internal/llm/gemini.go`:

```
Embed(ctx context.Context, text, taskType string) ([]float32, error)
```

- Model: `EMBEDDING_MODEL`, default `text-embedding-004`.
- Implemented with `client.Models.EmbedContent(ctx, model, genai.Text(text),
  &genai.EmbedContentConfig{TaskType: taskType})`.
- Returns `resp.Embeddings[0].Values` (a `[]float32` of length 768).
- Errors if there are zero embeddings or an empty vector.

Task types:

- `RETRIEVAL_DOCUMENT` when embedding a summary for storage.
- `RETRIEVAL_QUERY` when embedding the current prompt for search.

### 3.1 Summarization

Add to the same provider:

```
Summarize(ctx context.Context, text string) (string, error)
```

- Plain text-in / text-out generation using the default chat model
  (`gemini-2.5-flash`).
- Prompt instructs the model to extract durable, self-contained facts from a
  batch of messages, not to replay the conversation.

---

## 4. Repository

New file `internal/repository/mid_term_memory.go`.

```
type MemoryItem struct {
    Summary    string
    StartSeq   int64
    EndSeq     int64
    Similarity float64
}

type MidTermMemory struct { q *sqlc.Queries }

func NewMidTermMemory(pool *pgxpool.Pool) *MidTermMemory

func (m *MidTermMemory) Upsert(
    ctx context.Context,
    userID, summary string,
    embedding []float32,
    startSeq, endSeq int64,
    publishVersion int32,
) error

func (m *MidTermMemory) Search(
    ctx context.Context,
    userID string,
    embedding []float32,
    limit int,
) ([]MemoryItem, error)
```

- `Upsert` maps to the `UpsertMemory` query (see §5). Idempotent: a replayed or
  stale write updates in place, never duplicates.
- `Search` maps to the `SearchMemory` query and returns items ordered by
  similarity (best first). The caller applies a minimum-similarity threshold.

---

## 5. sqlc queries

New file `internal/db/queries/mid_term_memory.sql`.

```sql
-- name: UpsertMemory :exec
INSERT INTO mid_term_memory
    (user_id, publish_version, summary, embedding, start_seq, end_seq)
VALUES
    (@user_id, @publish_version, @summary, @embedding, @start_seq, @end_seq)
ON CONFLICT (user_id, publish_version) DO UPDATE SET
    summary    = EXCLUDED.summary,
    embedding  = EXCLUDED.embedding,
    start_seq  = EXCLUDED.start_seq,
    end_seq    = EXCLUDED.end_seq;

-- name: SearchMemory :many
SELECT
    user_id,
    publish_version,
    summary,
    start_seq,
    end_seq,
    1 - (embedding <=> @embedding) AS similarity
FROM mid_term_memory
WHERE user_id = @user_id
ORDER BY embedding <=> @embedding
LIMIT @limit;
```

### 5.1 sqlc config

`sqlc.yaml` gains an override so sqlc generates the `vector` column as the
pgvector type rather than `interface{}`:

```yaml
- db_type: "vector"
  go_type: "github.com/pgvector/pgvector-go.Vector"
```

The `@embedding` param is typed `pgvector.Vector` by this override; the
repository passes `pgvector.NewVector([]float32)`.

---

## 6. pgx type registration

`internal/db/db.go` registers the pgvector types with the pool by building a
`pgxpool.Config` and setting:

```go
config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
    return pgxvec.RegisterTypes(ctx, conn)
}
```

This is required for pgx to encode/decode the `vector` column; without it the
pool cannot read or write embeddings.

---

## 7. Mid-term layer

New file `internal/memory/mid-term.go`, implementing `pipeline.MemoryLayer`.

```go
type MidTermMemory struct {
    store    promotionStore   // ClaimBatch, MarkPromoted
    vector   vectorStore      // Upsert, Search
    summarize summarizer      // Summarize(ctx, text)
    embed    embedder         // Embed(ctx, text, taskType)
}
```

### 7.1 `Name()`

Returns `MidTermMemoryName` (`"MidTermMemory"`). The engine routes the doorbell
by `TargetLayer == Name()`, so this must match the `stage` short-term releases
to.

### 7.2 `RequestProcess`

1. Embed `chat.OriginalPrompt` with task type `RETRIEVAL_QUERY`.
2. `vector.Search(ctx, chat.UserID, embedding, topK)`.
3. Drop results below `minSimilarity`.
4. Add a `ContextBlock` with `Source`/`Tag` = `MidTermMemoryName`,
   `Priority` = 2 (below short-term's 1, above focus), content = each summary
   plus its `[start_seq, end_seq]` range so the AI can reference raw chat later.

Top-K and minimum similarity are constants (cf. `RecentMessageFloor`,
`PromotionBatchSize`), optionally overridable from the environment later.

### 7.3 `ResponseProcess`

No-op. Mid-term is fed by promotion, not per-turn.

### 7.4 `HandlePromotion`

Drain-to-quiescence loop (roadmap item 1):

```go
for {
    batch, err := store.ClaimBatch(ctx, event.UserID, MidTermMemoryName)
    if err != nil { return err }
    if len(batch.Messages) == 0 { return nil } // nothing left

    if err := summarizeAndStore(ctx, event.UserID, batch); err != nil { return err }

    if _, err := store.MarkPromoted(ctx, event.UserID, MidTermMemoryName,
        batch.TurnIDs(), batch.Version); err != nil { return err }
}
```

`summarizeAndStore`:

1. Render `batch.Messages` to text.
2. `summarize.Summarize(ctx, text)` → summary.
3. `embed.Embed(ctx, summary, "RETRIEVAL_DOCUMENT")` → embedding.
4. `vector.Upsert(ctx, userID, summary, embedding, first.Seq, last.Seq,
   batch.Version)` — where `first`/`last` are the lowest/highest `Seq` in the
   batch (messages arrive `seq`-ordered, so first and last elements).

---

## 8. Interface split (`internal/memory/memory.go`)

The single `conversationStore` interface is split so each layer depends only on
what it uses:

```go
type shortTermStore interface {
    AppendTurn(ctx, userID, prompt, response string) error
    ShortTermWindow(ctx, userID string, recentCount int) ([]repository.Message, error)
    ReleaseBatch(ctx, userID, stage string, recentFloor, batchSize int) (int64, error)
}

type promotionStore interface {
    ClaimBatch(ctx, userID, stage string) (repository.Batch, error)
    MarkPromoted(ctx, userID, stage string, turnIDs []uuid.UUID, version int32) (int64, error)
}

type vectorStore interface {
    Upsert(ctx, userID, summary string, embedding []float32, startSeq, endSeq int64, publishVersion int32) error
    Search(ctx, userID string, embedding []float32, limit int) ([]repository.MemoryItem, error)
}

type summarizer interface {
    Summarize(ctx context.Context, text string) (string, error)
}

type embedder interface {
    Embed(ctx context.Context, text, taskType string) ([]float32, error)
}
```

- `short-term.go` switches its field from `conversationStore` to
  `shortTermStore`.
- `long-term.go` is updated only enough to keep compiling (its placeholder
  `store` field's type follows the split); its real dependencies are decided in
  the long-term spec.

---

## 9. Wiring (`cmd/api/main.go`)

- Build `repository.NewMidTermMemory(pool)` for the vector store.
- The Gemini provider implements both `summarizer` and `embedder`; pass it to
  the layer for both.
- Append `memory.NewMidTermMemory(...)` to `memoryList` **after** short-term, so
  mid-term consumes what short-term releases.
- `db.Connect` already registers pgvector types (§6).

---

## 10. Environment / config

| Variable | Default | Meaning |
|---|---|---|
| `EMBEDDING_MODEL` | `text-embedding-004` | Embedding model for store + search |
| (future) `MEMORY_TOP_K` | `5` | Max summaries injected per request |
| (future) `MEMORY_MIN_SIMILARITY` | `0.3` | Drop results below this cosine similarity |

Only `EMBEDDING_MODEL` is read at this stage; Top-K and threshold are constants
first, promoted to env later if needed.

---

## 11. Verification checklist (implementation time)

1. `make migrate-up` creates `mid_term_memory` and the `vector` extension.
2. `make sqlc` generates `UpsertMemory` / `SearchMemory` with `pgvector.Vector`.
3. `go build ./...` compiles; `go vet ./...` clean.
4. After 16 messages for one user, mid-term claims and summarizes 8 at a time,
   promotes them, and the short-term window shrinks.
5. A backlog of several `published` batches drains from a single doorbell
   (drain-to-quiescence).
6. Re-running a summary for the same `(user_id, publish_version)` upserts rather
   than duplicating.
7. Retrieval returns only rows for the requesting `user_id`.

---

## 12. Out of scope

- **Long-term memory** — separate layer, future spec.
- **Raw-chat tool** — the tool the AI uses to fetch the real conversation from
  `start_seq`/`end_seq`; specified separately. This spec only persists and
  surfaces the range.
- **Recovery / reclaim** (roadmap Phase B) — unchanged by this spec.
- **Focus memory** (roadmap Phase C) — unchanged by this spec.
