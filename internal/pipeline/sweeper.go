package pipeline

import (
	"context"
	"log/slog"
	"time"
)

// SweepStore is the state a sweep acts on. It deals in primitives so the
// sweeper stays independent of the repository layer.
type SweepStore interface {
	IdleUsersWithBacklog(ctx context.Context, idleGap time.Duration) ([]string, error)
	ClaimIdleBacklog(ctx context.Context, userID string, idleGap time.Duration) (int64, error)
	UsersWithStalePublished(ctx context.Context, lease time.Duration) ([]string, error)
	ReclaimStalePublished(ctx context.Context, userID string, lease time.Duration) (int64, error)
}

type SweeperConfig struct {
	// Interval between sweeps. Both jobs are about state that changes on the
	// scale of minutes, so this wants to be coarse.
	Interval time.Duration

	// IdleGap is how long silence means a conversation is over. It has to match
	// whatever the memory layers use for session expiry, or the layers disagree
	// about whether the previous conversation is still happening.
	IdleGap time.Duration

	// Lease is how long a release may stay outstanding before it is presumed
	// abandoned. It must comfortably exceed a real ingest — extraction and
	// embedding are two model calls — because reclaiming a batch that is merely
	// slow makes the worker still holding it do its work for nothing.
	Lease time.Duration

	// SourceLayer and TargetLayer name the ends of the promotions this rings.
	SourceLayer string
	TargetLayer string
}

// Sweeper does the work that no request can trigger.
//
// Two jobs, both of which exist because the promotion path is driven entirely by
// turns arriving. A conversation that simply stops leaves a backlog under the
// threshold that nothing will ever release, and an ingest that dies leaves a
// release outstanding that blocks every later one for that user. Neither
// resolves itself, and both show up as a short-term window that grows without
// bound and keeps injecting a conversation the user finished days ago.
type Sweeper struct {
	store SweepStore
	pub   Publisher
	cfg   SweeperConfig
}

func NewSweeper(store SweepStore, pub Publisher, cfg SweeperConfig) *Sweeper {
	return &Sweeper{store: store, pub: pub, cfg: cfg}
}

// Run sweeps until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	slog.Info("sweeper started",
		"interval", s.cfg.Interval, "idle_gap", s.cfg.IdleGap, "lease", s.cfg.Lease)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Reclaim first: it moves abandoned releases back to pending, which
			// is exactly the state the idle flush below picks up, so a crashed
			// ingest for a finished conversation recovers within one tick
			// instead of two.
			s.reclaimAbandoned(ctx)
			s.flushIdle(ctx)
		}
	}
}

// flushIdle promotes the backlog of conversations that have ended.
func (s *Sweeper) flushIdle(ctx context.Context) {
	users, err := s.store.IdleUsersWithBacklog(ctx, s.cfg.IdleGap)
	if err != nil {
		slog.Error("sweep: find idle backlogs", "error", err)
		return
	}

	for _, userID := range users {
		released, err := s.store.ClaimIdleBacklog(ctx, userID, s.cfg.IdleGap)
		if err != nil {
			slog.Error("sweep: claim idle backlog", "user", userID, "error", err)
			continue
		}
		if released == 0 {
			// The session came back to life between the scan and the claim.
			continue
		}

		event := PromotionEvent{
			UserID:      userID,
			SourceLayer: s.cfg.SourceLayer,
			TargetLayer: s.cfg.TargetLayer,
		}
		if err := s.pub.Publish(ctx, event); err != nil {
			// Nothing is lost: the rows stay claimed, and the lease expiry above
			// returns them to the backlog for a later sweep to release again.
			slog.Warn("sweep: idle promotion not published",
				"user", userID, "messages", released, "error", err)
			continue
		}

		slog.Info("sweep: flushed idle backlog", "user", userID, "messages", released)
	}
}

// reclaimAbandoned returns releases nobody finished to the backlog.
func (s *Sweeper) reclaimAbandoned(ctx context.Context) {
	users, err := s.store.UsersWithStalePublished(ctx, s.cfg.Lease)
	if err != nil {
		slog.Error("sweep: find stale releases", "error", err)
		return
	}

	for _, userID := range users {
		n, err := s.store.ReclaimStalePublished(ctx, userID, s.cfg.Lease)
		if err != nil {
			slog.Error("sweep: reclaim stale release", "user", userID, "error", err)
			continue
		}
		if n > 0 {
			slog.Warn("sweep: reclaimed abandoned release", "user", userID, "messages", n)
		}
	}
}
