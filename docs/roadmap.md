# Roadmap

Status summary and the remaining work after Phase A. The full state machine and
design rationale live in [`memory-architecture.md`](./memory-architecture.md);
this file is the ordered plan, meant to be picked up on a fresh checkout.

## Done (Phase A — bounded batching + worker pool)

- State machine `pending → published → processing → promoted` (+ `reclaim`).
- Columns: `stage` (value `MidTermMemoryName` = `"MidTermMemory"`), `claimed_at`.
- `ReleaseBatch` — bounded (8), always-emit, versioned; condition is
  `count(pending) - floor >= batch_size`.
- `ClaimBatch` — atomic oldest-first claim (`min(publish_version)`).
- `MarkPromoted` — ack from `processing`, fenced by `stage` + `publish_version`.
- Worker pool (N dispatchers, `PROMOTION_WORKERS`, default 4).
- Non-blocking `publishPromotable` (detached context + timeout).
- `docs/memory-architecture.md` spec.

Key symbols (referenced below by name, not line):

| Package | Symbol |
|---|---|
| `repository` | `ReleaseBatch(ctx, userID, stage string, recentFloor, batchSize int) (int64, error)` |
| `repository` | `ClaimBatch(ctx, userID, stage string) (Batch, error)` — `Batch{Messages, Version}`, `Batch.TurnIDs()` |
| `repository` | `MarkPromoted(ctx, userID, stage string, turnIDs []uuid.UUID, version int32) (int64, error)` |
| `memory` | `MidTermMemoryName = "MidTermMemory"`, `RecentMessageFloor = 8`, `PromotionBatchSize = 8` |
| `pipeline` | `MemoryLayer` interface: `Name / RequestProcess / ResponseProcess / HandlePromotion` |
| `pipeline` | `PromotionEvent{UserID, SourceLayer, TargetLayer}`, `Publisher` |

---

## 1. Consumer loop (drain-to-quiescence)

**Problem.** "One release → one event → one batch claimed" means a lost-event
backlog never drains: if 5 batches are `published` but their doorbells were
consumed/lost, the next single event only claims the oldest, leaving 4 stuck.

**Decision (made).** The consumer must drain to quiescence — loop until
`ClaimBatch` returns empty. No "emit N events" (the `min(version)` claim makes
N concurrent workers collide on the oldest batch and all but one get empty).

**Task.** In the mid-term layer's `HandlePromotion` (see item 2):

```go
for {
    batch, err := store.ClaimBatch(ctx, userID, stage)
    if err != nil { return err }
    if len(batch.Messages) == 0 { return nil } // nothing left
    if err := summarize(batch); err != nil { return err }
    if _, err := store.MarkPromoted(ctx, userID, stage, batch.TurnIDs(), batch.Version); err != nil {
        return err
    }
}
```

This is implemented as part of item 2 (there is no consumer yet), but the loop
shape is the fix for this specific bug and must not be dropped.

---

## 2. Mid-term layer

The missing consumer. `HandlePromotion` is currently a no-op and `ClaimBatch`/
`MarkPromoted` have no caller — the pipeline is plumbed but nothing summarizes.

**Open decision (blocking):** vector store. Options:
- `pgvector` extension in the existing Postgres (least infra, one DB), or
- a separate service (Qdrant / Pinecone / etc.).

Pick this before writing the store.

**Tasks.**
- [ ] New `internal/memory/mid-term.go` implementing `pipeline.MemoryLayer`.
  - `Name()` → `"MidTermMemory"` (must equal `MidTermMemoryName`; the engine
    routes the doorbell by `TargetLayer == Name()`).
  - `RequestProcess` → retrieve relevant mid-term context from the vector store
    and add a context block (priority between short-term and focus).
  - `ResponseProcess` → no-op (mid-term is fed by promotion, not per-turn).
  - `HandlePromotion` → the drain-to-quiescence loop from item 1.
- [ ] Summarization: an LLM call that extracts durable facts from a batch
    (`Batch.Messages`), not raw conversation replay.
- [ ] Vector store write, **idempotent keyed by `(user_id, publish_version)`** —
    a replayed/stale write must upsert, never duplicate or clobber.
- [ ] Wire into `main.go` after short-term; keep `memoryList` order: short-term
    first, then mid-term (mid-term consumes what short-term releases).
- [ ] Extend the `conversationStore` split: short-term needs `ReleaseBatch`;
    mid-term needs `ClaimBatch` + `MarkPromoted`. Consider two narrow interfaces
    instead of one fat one.

**Acceptance.** After 16 messages for one user, mid-term claims and summarizes
8 at a time, promotes them, and the short-term window shrinks accordingly;
a backlog of several `published` batches drains from a single doorbell.

---

## 3. Recovery (Phase B)

Two distinct failure modes, both solved by a periodic sweep.

**3a. Reclaim** — a worker crashes mid-summary, leaving rows stuck in
`processing`.

**3b. Reconciliation** — rows sit `published` because every doorbell for them
was lost (queue full, crash before publish, system down). This is the
"zero events" half that the consumer loop cannot fix.

**Decisions (made):** reclaim bumps `publish_version` on `processing →
published` (revokes the stale worker's fence token); the sweep re-emits a
doorbell for affected users.

**Tasks.**
- [ ] `ReclaimStale` query: `processing → published` where
    `claimed_at < now() - @lease`, bumping `publish_version`.
- [ ] Reconciliation query: distinct users with rows `published` (and/or stale
    `processing`), so the sweep knows whom to re-ring.
- [ ] Sweep goroutine (periodic, e.g. every 30s–60s) that runs both and
    publishes a `PromotionEvent` per affected user.
- [ ] Slow-worker cancellation: bound the summarization call by the lease so a
    stalled worker aborts (context deadline) rather than being reclaimed after
    the fact.
- [ ] Lease timeout as env config (`RECLAIM_LEASE`, default ~5m) — must exceed
    worst-case summarization latency.

**Open decision:** sweep interval and exact lease default.

---

## 4. Focus memory (Phase C)

A one-sentence subject of the current conversation, extracted asynchronously.

**Tasks.**
- [ ] Migration `00004`: `focus` table —
    `user_id text PRIMARY KEY, subject text NOT NULL,
     updated_at timestamptz NOT NULL DEFAULT now(), last_turn_seq bigint NOT NULL`.
- [ ] LLM `Extractor` interface (`Extract(ctx, prompt) (string, error)`) on the
    Gemini provider — a small dedicated "state the subject in one sentence" call.
- [ ] `FocusMemory` layer:
  - `RequestProcess` → `GetFocus(userID)`, inject as highest-priority block.
  - `ResponseProcess` → snapshot `chat.RenderMemory()` + the new response,
    enqueue a coalesced extraction (non-blocking).
  - `HandlePromotion` → no-op.
- [ ] Repository: `GetFocus` / `SetFocus` with a **version-guarded upsert** —
    `ON CONFLICT (user_id) DO UPDATE ... WHERE focus.last_turn_seq < @last_turn_seq`
    so a stale extraction never overwrites a fresher one. Stamp with the turn's
    `seq` (from `AppendTurn` returning `seq`, or read `max(seq)`).
- [ ] Async coalescing worker: bounded pool + per-user single-flight
    ("cancel previous, keep latest") so at most one extraction is in flight per
    user and only the newest wins.

---

## Decisions locked (do not re-litigate)

- `stage` value is `MidTermMemoryName` (`"MidTermMemory"`); it lives in `memory`,
    not `repository`. The repository treats `stage` as an opaque string.
- Batching: `RecentMessageFloor = 8`, `PromotionBatchSize = 8`; release fires
    when `count(pending) - floor >= batch`.
- Reclaim bumps `publish_version`.
- Consumer drains to quiescence (no "emit N events").

## Open decisions

1. Vector store choice for mid-term (pgvector vs external service).
2. Sweep interval + `RECLAIM_LEASE` default.
3. Whether `AppendTurn` should return the turn `seq` (for focus's `last_turn_seq`).

## Gotcha (sqlc)

Never write `@a + @b` (arithmetic between two named params) in a query. `@` is
a Postgres prefix operator, so `@a + @b` parses as `@ (a + @b)` and sqlc's
param rewrite corrupts the SQL (obscure `syntax error at or near ...`). Use
`sqlc.arg()` for the arithmetic, or restructure to one param (e.g.
`count - @floor::bigint >= @batch::bigint`). The `::bigint` cast is needed to
force `int64` inference (otherwise params generate as `interface{}`/`int32`).
