package memory

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/shahab5191/memshin/internal/pipeline"
)

// FocusMemory tracks what the conversation is currently about.
//
// It exists to make references resolvable. "Is that still fine?" carries no
// retrievable content on its own, and a fixed window of prior messages is the
// wrong fix — too little and anaphora stays unresolved, too much and a topic
// change drags retrieval back toward what the user just left. An explicit set of
// current topics is what the decomposer needs, and it is a set rather than a
// single topic because a conversation is often genuinely about two things.
//
// The set is written to ChatContext for layers after it and rendered as its own
// block for the model, which are different jobs: mid-term consumes it
// programmatically, the model reads it as framing.
type FocusMemory struct {
	store    focusStore
	analyst  focusAnalyst
	idleGap  time.Duration
	capacity int
}

func NewFocusMemory(store focusStore, analyst focusAnalyst) *FocusMemory {
	return &FocusMemory{
		store:    store,
		analyst:  analyst,
		idleGap:  SessionIdleGap,
		capacity: FocusCapacity,
	}
}

func (fm *FocusMemory) Name() string {
	return FocusMemoryName
}

// RequestProcess loads the current focus, ending the session first if it has
// gone cold, and publishes it both to the pipeline and to the prompt.
func (fm *FocusMemory) RequestProcess(ctx context.Context, chat *pipeline.ChatContext) error {
	phrases, sessionEnded, err := fm.store.Current(ctx, chat.UserID, fm.idleGap, fm.capacity)
	if err != nil {
		// Focus sharpens retrieval; it does not gate it. Downstream layers see
		// an empty set and work from the prompt alone.
		slog.Error("focus unavailable",
			"layer", fm.Name(), "user", chat.UserID, "error", err)
		return nil
	}
	if sessionEnded {
		slog.Info("focus session expired",
			"layer", fm.Name(), "user", chat.UserID, "idle_gap", fm.idleGap)
	}
	if len(phrases) == 0 {
		return nil
	}

	// Pipeline state, for layers that consume it as data — mid-term hands it to
	// the decomposer to resolve references before probing.
	chat.Focus = phrases

	chat.AddBlock(pipeline.ContextBlock{
		Source: fm.Name(),
		Tag:    FocusMemoryTag,
		// Last of the three: mid-term is recalled background, short-term is the
		// live thread, and this is a statement about the thread's current
		// subject, which reads best immediately before the turn itself.
		Priority: 2,
		Content:  renderFocus(phrases),
	})

	return nil
}

// ResponseProcess updates the focus set from the exchange that just happened.
//
// It runs detached. Engine.Process waits on every ResponseProcess before
// returning the answer, so extracting inline would put a Flash Lite call on the
// user's latency after their response was already generated. The cost is that a
// turn arriving before extraction finishes reads focus one turn stale — a
// weaker probe, not a wrong one — and typing time makes that rare.
func (fm *FocusMemory) ResponseProcess(
	_ context.Context,
	chat *pipeline.ChatContext,
	llmResponse string,
	_ pipeline.Publisher,
) error {
	// Deliberately not derived from the request context: that is cancelled the
	// moment the handler returns, which is immediately after this call.
	userID, prompt, current := chat.UserID, chat.OriginalPrompt, chat.Focus

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), focusUpdateTimeout)
		defer cancel()

		phrases, err := fm.analyst.ExtractFocus(ctx, prompt, llmResponse, current)
		if err != nil {
			slog.Warn("focus not updated: extract",
				"layer", fm.Name(), "user", userID, "error", err)
			return
		}
		if len(phrases) == 0 {
			return
		}

		if err := fm.store.Reinforce(ctx, userID, phrases, fm.capacity); err != nil {
			slog.Error("focus not updated: reinforce",
				"layer", fm.Name(), "user", userID, "error", err)
			return
		}

		slog.Debug("focus updated",
			"layer", fm.Name(), "user", userID, "topics", len(phrases))
	}()

	return nil
}

// HandlePromotion is a no-op: focus is derived from the live conversation and
// has nothing to receive from an upstream layer.
func (fm *FocusMemory) HandlePromotion(
	_ context.Context,
	_ pipeline.PromotionEvent,
	_ pipeline.Publisher,
) error {
	return nil
}

func renderFocus(phrases []string) string {
	var b strings.Builder
	b.WriteString("Currently discussing:\n")
	for i, p := range phrases {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(p)
	}
	return b.String()
}
