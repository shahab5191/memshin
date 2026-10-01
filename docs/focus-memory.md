# Focus Memory — Spec

Status: **spec** — not yet implemented.

This document specifies the focus layer (roadmap Phase C): a single, per-user
sentence that captures what the conversation is about *right now*. It is
refreshed after every turn by asking the LLM to keep, refine, or replace the
current subject, stored append-only so a history of subjects survives, and used
as the retrieval query for mid-term memory so the vector search recalls facts
relevant to the current topic rather than to a possibly-terse prompt alone.

---

## 1. Context and decisions

### 1.1 Naming

The subject lives in the **focus** layer. The table is `focus` and the layer is
`FocusMemory` (`FocusMemoryName = "FocusMemory"`). The stub at
`internal/memory/focus-memory.go` is replaced by the real layer.

### 1.2 Locked decisions

| Decision | Value |
|---|---|
| Granularity | one sentence per user, refreshed each turn |
| Storage | append-only `focus` table; query latest, never update in place |
| Recency | latest = highest `last_turn_seq` (not highest `id`) |
| Trigger | content-free doorbell (`PromotionEvent` → `FocusMemoryName`) after each response |
| Extraction input | current subject + latest exchange (latest prompt + response) |
| Extraction output | JSON `{ keep: bool, content: string }` |
| Concurrency | per-user single-flight: cancel in-flight, run the newest |
| Retrieval query (mid-term) | `focus + "\n" + current prompt` |
| Write policy | insert only when the subject changes (`keep == false`) |

### 1.3 Why append-only

Focus is not a single mutable value to be overwritten. Keeping one row per
subject — stamped with the `last_turn_seq` at which it became current — yields a
free timeline of "this was the subject from turn N". That history is cheap now
and potentially useful later (topic drift analysis, session summaries,
long-term distillation). Querying the latest is a single indexed fetch
(`user_id, last_turn_seq DESC`).

### 1.4 Why "latest" is `last_turn_seq`, not `id`

Extractions are asynchronous and can finish out of order. A stale extraction for
an older turn may insert its row *after* a fresher one, and would then hold the
higher `id`. Ordering by `id` would therefore return the stale subject. Ordering
by `last_turn_seq DESC` (with `id` as a tiebreak) makes the recency marker
explicit and immune to completion order. `id` exists for identity and history,
not for recency.

### 1.5 Why a doorbell + single-flight

Extraction is an LLM call off the request path. The trigger is a content-free
doorbell (`PromotionEvent`), reusing the existing `Publisher` and engine
dispatch: focus reads the latest exchange from the database at action time, so
the event carries no payload and redelivery is idempotent.

A fast typist can produce several turns before one extraction completes. Without
coordination that would mean several concurrent LLM calls for the same user —
wasted cost and a race on which subject is "current". The focus layer therefore
keeps at most one extraction in flight per user, and a new doorbell **cancels**
the running one and starts the newest. The `last_turn_seq` ordering in §1.4 is
the backstop that keeps the result correct even if a cancellation ever slips
through.

---

## 2. Schema

Migration `00005_create_focus.sql`.

```sql
-- +goose Up
CREATE TABLE focus (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       text        NOT NULL,
    subject       text        NOT NULL,
    last_turn_seq bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX focus_user_latest_idx ON focus (user_id, last_turn_seq DESC);

-- +goose Down
DROP TABLE focus;
```

Notes:

- `id` is identity only — never used for ordering (§1.4).
- `last_turn_seq` references `conversation.seq` of the exchange the subject was
  extracted from (the response message's `seq`). It is a recency/history marker,
  not a foreign key: no constraint is declared, mirroring how `mid_term_memory`
  records `start_seq`/`end_seq` as plain `bigint`.
- `focus_user_latest_idx` serves the single hot path: latest subject per user.

---

## 3. LLM extraction

Add to the Gemini provider in `internal/llm/gemini.go`:

```
Extract(ctx context.Context, currentSubject, text string) (keep bool, subject string, err error)
```

- Plain text-in, structured-out generation using the default chat model
  (`gemini-2.5-flash`).
- `ResponseMIMEType = "application/json"` and a `ResponseSchema` enforcing
  `{ "keep": bool, "content": string }` (both required).
- `text` is the latest exchange rendered as `prompt + "\n" + response`; the
  current subject is passed separately so the model can decide to keep, refine,
  or replace it.
- Prompt instructs the model to return the one-sentence subject *now* — restate
  it unchanged if it still holds, refine it, or replace it if the topic changed.
  On the first turn `currentSubject` is empty and `keep` is meaningless (the
  layer still inserts whatever `content` comes back).

The existing per-call timeout applies. No new environment variables are needed;
a dedicated focus model/temperature is out of scope (see §10).

---

## 4. Repository

### 4.1 `internal/repository/focus.go`

```
func NewFocus(pool *pgxpool.Pool) *Focus

func (f *Focus) GetFocus(ctx context.Context, userID string) (string, error)
func (f *Focus) InsertFocus(ctx context.Context, userID, subject string, lastTurnSeq int64) error
```

- `GetFocus` maps to `GetFocus` (see §5) and returns the latest subject, or the
  empty string when the user has none (`pgx.ErrNoRows` → `""`, no error).
- `InsertFocus` maps to `InsertFocus` (see §5). Append-only: always a new row,
  never an upsert.

### 4.2 `internal/repository/conversation.go`

Add:

```
func (c *Conversations) LatestExchange(ctx context.Context, userID string) ([]Message, error)
```

- Maps to `LatestExchange` (see §5): the user's last two messages, ordered by
  `seq` ascending — the most recent exchange. `[0]` is the prompt, `[1]` is the
  response, and `lastTurnSeq` for the insert is `messages[1].Seq`.
- A turn is appended atomically as two messages (`AppendTurn`), so after a
  response has been recorded this always returns a complete pair.

---

## 5. sqlc queries

New file `internal/db/queries/focus.sql`:

```sql
-- name: GetFocus :one
SELECT subject
FROM focus
WHERE user_id = @user_id
ORDER BY last_turn_seq DESC, id DESC
LIMIT 1;

-- name: InsertFocus :exec
INSERT INTO focus (user_id, subject, last_turn_seq)
VALUES (@user_id, @subject, @last_turn_seq);
```

Add to `internal/db/queries/conversation.sql`:

```sql
-- name: LatestExchange :many
SELECT seq, id, user_id, turn_id, role, content, created_at
FROM conversation
WHERE user_id = @user_id
ORDER BY seq DESC
LIMIT 2;
```

---

## 6. Focus layer

Replace the stub at `internal/memory/focus-memory.go` with the implementation of
`pipeline.MemoryLayer`.

```go
type FocusMemory struct {
    source  focusSource   // LatestExchange
    store   focusStore    // GetFocus, InsertFocus
    extract extractor     // Extract(ctx, currentSubject, text)

    mu     sync.Mutex
    cancel map[string]context.CancelFunc
}
```

### 6.1 `Name()`

Returns `FocusMemoryName` (`"FocusMemory"`). The engine routes the doorbell by
`TargetLayer == Name()`, so this must match the target the layer publishes to
(§6.3).

### 6.2 `RequestProcess`

1. `store.GetFocus(ctx, chat.UserID)`.
2. If non-empty, set `chat.FocusSubject` (the field mid-term reads in §7) and add
   a `ContextBlock` with `Source`/`Tag` = `FocusMemoryName`, `Priority` = 0
   (above short-term's 1 and mid-term's 2), content = the subject sentence.
3. If empty (first turn — nothing extracted yet), do nothing.

### 6.3 `ResponseProcess`

Publish a doorbell so the extraction happens off the request path:

```
PromotionEvent{ UserID, SourceLayer: FocusMemoryName, TargetLayer: FocusMemoryName }
```

- Non-blocking via the existing `Publisher` (drops with
  `ErrPromotionQueueFull` like the other layers; a lost doorbell self-heals
  because the next turn rings again).
- Must run **after** short-term's `AppendTurn` (see §8 ordering), so the exchange
  is durable before the doorbell fires.

### 6.4 `HandlePromotion`

Per-user single-flight with cancellation:

```go
func (f *FocusMemory) HandlePromotion(ctx, event, pub) error {
    f.mu.Lock()
    if cancel, ok := f.cancel[event.UserID]; ok {
        cancel() // cancel the in-flight extraction
    }
    userCtx, cancel := context.WithCancel(ctx)
    f.cancel[event.UserID] = cancel
    f.mu.Unlock()

    defer func() {
        f.mu.Lock()
        delete(f.cancel, event.UserID)
        f.mu.Unlock()
    }()

    subject, err := f.store.GetFocus(userCtx, event.UserID)
    if err != nil { return err }

    exchange, err := f.source.LatestExchange(userCtx, event.UserID)
    if err != nil { return err }
    if len(exchange) < 2 { return nil } // nothing to extract yet

    text := string(exchange[0].Role) + ": " + exchange[0].Content + "\n" +
            string(exchange[1].Role) + ": " + exchange[1].Content

    keep, next, err := f.extract.Extract(userCtx, subject, text)
    if err != nil {
        if errors.Is(err, context.Canceled) {
            return nil // superseded by a newer doorbell
        }
        return err
    }

    // First extraction has no subject to keep; a change always inserts.
    if !keep || subject == "" {
        return f.store.InsertFocus(userCtx, event.UserID, next, exchange[1].Seq)
    }
    return nil
}
```

Notes:

- The `cancel` map is the single-flight mechanism: at most one extraction per
  user, and a newer doorbell aborts the older one. The engine may run several
  dispatcher workers; the mutex serialises the map access, not the LLM call.
- `context.Canceled` is not a real failure — swallow it so a superseded
  extraction does not log as an error.
- Write policy (§1.2): insert only on change (`keep == false`), except the first
  subject which always inserts. History is therefore one row per *distinct*
  subject, not one per turn.

---

## 7. Mid-term retrieval change (`internal/memory/mid-term.go`)

`RequestProcess` embeds the focus subject together with the prompt so the vector
search is topic-scoped:

```go
query := chat.OriginalPrompt
if chat.FocusSubject != "" {
    query = chat.FocusSubject + "\n" + chat.OriginalPrompt
}
embedding, err := mtm.embed.Embed(ctx, query, retrievalQueryTaskType)
```

No other mid-term behaviour changes. The fallback (no subject yet, or focus
unavailable) is the current prompt-only query. No caching: the query changes
every turn (the prompt half is always new), so there is nothing stable to cache.

---

## 8. Interface split and wiring

### 8.1 `internal/memory/memory.go`

Add to the split introduced for mid-term:

```go
type focusSource interface {
    LatestExchange(ctx context.Context, userID string) ([]repository.Message, error)
}

type focusStore interface {
    GetFocus(ctx context.Context, userID string) (string, error)
    InsertFocus(ctx context.Context, userID, subject string, lastTurnSeq int64) error
}

type extractor interface {
    Extract(ctx context.Context, currentSubject, text string) (keep bool, subject string, err error)
}
```

`repository.Conversations` satisfies `focusSource`; `repository.Focus`
satisfies `focusStore`; the Gemini provider satisfies `extractor`.

### 8.2 `pipeline/context.go`

`ChatContext` gains `FocusSubject string`, set by the focus layer in
`RequestProcess` and read by the mid-term layer in the same request. This keeps
the two layers decoupled from focus's storage — mid-term never queries the
`focus` table directly.

### 8.3 `cmd/api/main.go`

Build `repository.NewFocus(pool)` and append the layers in order:

```
short-term → focus → mid-term
```

Ordering matters twice:

- `RequestProcess`: focus must run before mid-term so `chat.FocusSubject` is set
  before the retrieval query is built.
- `ResponseProcess`: focus must run after short-term so the turn is appended
  (`AppendTurn`) before focus rings its doorbell.

---

## 9. Environment / config

No new variables. Extraction reuses the chat model and its timeout. A dedicated
focus model/temperature/timeout is a possible future addition, not part of this
spec.

---

## 10. Verification checklist (implementation time)

1. `make migrate-up` creates `focus` and `focus_user_latest_idx`.
2. `make sqlc` generates `GetFocus` / `InsertFocus` / `LatestExchange`.
3. `go build ./...` compiles; `go vet ./...` clean.
4. After turn 1 for a user, a focus row is inserted (via the doorbell); the
   turn-2 request injects a Priority-0 subject block and mid-term embeds
   `subject + "\n" + prompt`.
5. Sending several turns quickly yields at most one in-flight extraction per
   user (older one canceled); history accumulates one row per distinct subject.
6. A stale extraction finishing late never becomes "latest" — `GetFocus` returns
   the highest `last_turn_seq`, not the highest `id`.
7. First turn (no subject yet) still works: no focus block, prompt-only
   retrieval, and the first extraction always inserts.

---

## 11. Out of scope

- **Long-term memory** — separate layer, future spec.
- **Raw-chat tool** — unchanged from the mid-term spec.
- **Recovery / reclaim** (roadmap Phase B) — unchanged.
- **Focus model/temperature/timeout config** — reuses chat defaults.
- **Caching of retrieval results** — deliberately omitted; the query changes
  every turn so there is nothing stable to cache.
