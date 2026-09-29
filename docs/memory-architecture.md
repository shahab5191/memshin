# Memory Architecture

memshin is a multi-layer memory system for an LLM. Each request flows through
memory layers that (a) inject recalled context before generation and (b) record
and promote the turn after generation.

## Layers

- **Short-term** — the raw, un-summarised live thread. Injects the current
  conversation into every request and, as it grows past a floor, releases older
  messages downstream in bounded batches.
- **Mid-term** (planned) — extracts durable facts from each batch, stores them
  in a vector DB, and returns the subset relevant to a request instead of
  feeding the whole session back in.
- **Focus** (planned) — a one-sentence subject of the current conversation,
  extracted asynchronously after each response.
- **Long-term** (future) — compact, rarely-changing knowledge distilled from
  mid-term.

## State machine

Rows in `conversation` move through a per-user lifecycle:

```
pending ──release──▶ published ──claim──▶ processing ──ack──▶ promoted
                         ▲                        │
                         └─────reclaim (lease)────┘
```

| Status      | Meaning                                                       | Set by                  |
|-------------|---------------------------------------------------------------|-------------------------|
| `pending`   | In the short-term backlog, not yet released downstream.       | `AppendTurn` (default)  |
| `published` | Released to a layer, awaiting a worker to claim it.           | `ReleaseBatch`          |
| `processing`| Claimed by a worker, being summarised.                        | `ClaimBatch`            |
| `promoted`  | Durably stored downstream and acknowledged.                   | `MarkPromoted`          |

`stage` scopes each row to the layer that may claim it, so a consumer of one
layer cannot accidentally claim rows released to another. `publish_version` is
the batch generation, bumped on every release and every reclaim; it is the
fence that makes a stale worker's acknowledgement match zero rows.

## Batching and versioning

- A release is bounded to `PromotionBatchSize` messages and fires only once a
  full batch has accumulated above `RecentMessageFloor`. Mid-term is therefore
  never handed a couple of messages per turn, and never handed an unbounded
  backlog at once.
- Each release stamps a fresh, monotonically increasing `publish_version`.
- A worker claims the oldest unclaimed batch — `min(publish_version)` — by
  atomically transitioning `published → processing` under the row lock. Two
  workers can never claim the same batch; the loser gets an empty result and
  exits without a summarisation.
- Mid-term stores extractions keyed by version, so completion order does not
  matter. Oldest-first is a best-effort policy (it keeps summaries roughly
  chronological), not a correctness invariant.

## Doorbell events

Promotion events are content-free doorbells: `{UserID, SourceLayer,
TargetLayer}`. The producer releases rows in the database, then rings the bell;
the target reads the outstanding rows itself. This keeps events trivially
serialisable and idempotent under redelivery — a redelivered doorbell re-reads
state and finds nothing new, where a redelivered payload would be written
twice.

## Phases

### Phase A — bounded batching + worker pool (current)

- Migration `00003`: add `stage`, the `processing` status, and `claimed_at`.
- `ReleaseBatch` (replaces `ClaimPromotable`): always-emit bounded batch, no
  "earlier release outstanding" gate.
- `ClaimBatch`: atomic oldest-first claim (`published → processing`).
- `MarkPromoted`: acknowledge from `processing`, fenced by `stage` +
  `publish_version`.
- `publishPromotable` runs non-blocking on a detached context.
- The engine drains its channel with a pool of N workers; the database claim,
  not the goroutine count, guarantees exactly-once summarisation.

### Phase B — recovery

- Reclaim sweep: a periodic job flips `processing → published` (bumping
  `publish_version`) for rows whose `claimed_at` is older than the lease, then
  re-emits the doorbell for the affected users.
- Slow-worker cancellation: the summarisation call is bounded by the lease so a
  stalled worker aborts rather than being reclaimed after the fact.

### Phase C — focus

- New `FocusMemory` layer: `RequestProcess` injects the current subject as the
  highest-priority block; `ResponseProcess` snapshots the assembled context and
  enqueues an async extraction (coalesced per user).
- A bounded worker pool calls the LLM to extract a one-sentence subject and
  persists it in a `focus` table (one row per user), guarded by a monotonic
  `last_turn_seq` so a stale extraction never overwrites a fresher one.
