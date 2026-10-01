package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/shahab5191/memshin/internal/promotion"
)

// FocusMemory holds the current one-sentence subject of a user's conversation,
// refreshed asynchronously after every response and used by mid-term to scope
// its retrieval query.
type FocusMemory struct {
	source  focusSource // ShortTermWindow
	store   focusStore  // GetFocus, InsertFocus
	extract extractor   // Extract

	// mu guards cancel; the map is the per-user single-flight mechanism, so the
	// mutex serialises map access, never the LLM call itself.
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

func NewFocusMemory(source focusSource, store focusStore, extract extractor) *FocusMemory {
	return &FocusMemory{
		source:  source,
		store:   store,
		extract: extract,
		cancel:  make(map[string]context.CancelFunc),
	}
}

func (fm *FocusMemory) Name() string {
	return FocusMemoryName
}

// RequestProcess injects the current subject as the highest-priority block and
// records it on the context for mid-term to embed. First turn has no subject yet
// and injects nothing.
func (fm *FocusMemory) RequestProcess(ctx context.Context, chat *ChatContext) error {
	subject, err := fm.store.GetFocus(ctx, chat.UserID)
	if err != nil {
		return fmt.Errorf("%s: get focus: %w", fm.Name(), err)
	}
	if subject == "" {
		return nil
	}

	chat.FocusSubject = subject
	chat.AddBlock(ContextBlock{
		Source:   fm.Name(),
		Tag:      FocusMemoryName,
		Content:  subject,
		Priority: 0,
	})

	return nil
}

// ResponseProcess rings the doorbell that triggers extraction off the request
// path. It runs after short-term's AppendTurn (see main.go ordering), so the
// window is durable before the doorbell fires. A dropped doorbell self-heals:
// the next turn rings again.
func (fm *FocusMemory) ResponseProcess(
	ctx context.Context,
	chat *ChatContext,
	llmResponse string,
	pub promotion.Publisher,
) error {
	if pub == nil {
		return nil
	}

	event := promotion.Event{
		UserID:      chat.UserID,
		SourceLayer: fm.Name(),
		TargetLayer: fm.Name(),
	}
	if err := pub.Publish(ctx, event); err != nil {
		slog.Warn("focus promotion not published",
			"layer", fm.Name(), "user", chat.UserID, "error", err)
	}
	return nil
}

// HandlePromotion extracts the current subject from the full short-term window.
// At most one extraction runs per user: a newer doorbell cancels the in-flight
// one, and the last_turn_seq ordering in the store is the backstop that keeps a
// stale result from ever being treated as latest.
func (fm *FocusMemory) HandlePromotion(
	ctx context.Context,
	event promotion.Event,
	pub promotion.Publisher,
) error {
	fm.mu.Lock()
	if cancel, ok := fm.cancel[event.UserID]; ok {
		cancel() // supersede the in-flight extraction
	}
	userCtx, cancel := context.WithCancel(ctx)
	fm.cancel[event.UserID] = cancel
	fm.mu.Unlock()

	defer func() {
		fm.mu.Lock()
		delete(fm.cancel, event.UserID)
		fm.mu.Unlock()
	}()

	subject, err := fm.store.GetFocus(userCtx, event.UserID)
	if err != nil {
		return err
	}

	window, err := fm.source.ShortTermWindow(userCtx, event.UserID, RecentMessageFloor)
	if err != nil {
		return err
	}
	if len(window) == 0 {
		return nil // nothing to extract from yet
	}

	keep, next, err := fm.extract.Extract(userCtx, subject, renderMessages(window))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil // superseded by a newer doorbell — not a failure
		}
		return err
	}

	// First extraction has no subject to keep; a change always inserts. History
	// is therefore one row per distinct subject, not one per turn.
	if !keep || subject == "" {
		return fm.store.InsertFocus(userCtx, event.UserID, next, window[len(window)-1].Seq)
	}
	return nil
}
