package pipeline

import (
	"context"
	"errors"
	"log"
	"log/slog"
)

// promotionBuffer is generous because an event is three strings. Depth is
// bounded in practice by the number of users with an outstanding promotion,
// so overflow means the dispatcher has stalled, not that the queue is small.
const promotionBuffer = 4096

// ErrPromotionQueueFull means the event was not queued. Nothing is lost by it:
// the source layer's rows stay claimed in the database, so the next doorbell
// for that user — or the reclaim sweep — picks the work up again.
var ErrPromotionQueueFull = errors.New("promotion queue full")

type MemoryLayer interface {
	Name() string
	RequestProcess(ctx context.Context, chat *ChatContext) error
	ResponseProcess(ctx context.Context, chat *ChatContext, llmResponse string, pub Publisher) error
	HandlePromotion(ctx context.Context, event PromotionEvent, pub Publisher) error
}

type LLMProvider interface {
	Name() string
	GenerateResponse(ctx context.Context, chat *ChatContext) (string, error)
}

type Engine struct {
	memory []MemoryLayer
	llm    LLMProvider
	promCh chan PromotionEvent
	pub    Publisher
}

func NewEngine(memory []MemoryLayer, llm LLMProvider) *Engine {
	promCh := make(chan PromotionEvent, promotionBuffer)
	return &Engine{
		memory: memory,
		llm:    llm,
		promCh: promCh,
		pub:    NewChannelPublisher(promCh),
	}
}

func (e *Engine) RunPromotions(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-e.promCh:
			if !ok {
				return
			}
			e.dispatch(ctx, event)
		}
	}
}

func (e *Engine) dispatch(ctx context.Context, event PromotionEvent) {
	for _, layer := range e.memory {
		if layer.Name() != event.TargetLayer {
			continue
		}
		if err := layer.HandlePromotion(ctx, event, e.pub); err != nil {
			slog.Error("promotion handler failed",
				"layer", layer.Name(), "user", event.UserID, "error", err)
		}
		return
	}

	slog.Warn("promotion event addressed to unknown layer",
		"target", event.TargetLayer, "source", event.SourceLayer, "user", event.UserID)
}

func (e *Engine) Process(ctx context.Context, userID, prompt, sysMsg string) (string, error) {
	chat := &ChatContext{
		UserID:         userID,
		OriginalPrompt: prompt,
		SystemMessage:  sysMsg,
		Blocks:         []ContextBlock{},
	}

	// step 1: assemble context from the memory layers
	for _, layer := range e.memory {
		log.Println("processing request through memory layer", layer.Name())
		if err := layer.RequestProcess(ctx, chat); err != nil {
			log.Println("memory layer failed to process request", err)
			return "", err
		}
	}

	// step 2: generate the response
	log.Println("generating response through LLM", e.llm.Name())
	r, err := e.llm.GenerateResponse(ctx, chat)
	if err != nil {
		log.Println("LLM failed to generate response", err)
		return "", err
	}

	// step 3: let the layers record the turn.
	log.Println("processing response through memory layers")
	for _, layer := range e.memory {
		log.Println("processing response through memory layer", layer.Name())
		if err := layer.ResponseProcess(ctx, chat, r, e.pub); err != nil {
			slog.Error("memory layer failed to record turn",
				"layer", layer.Name(), "user", userID, "error", err)
		}
	}

	return r, nil
}

// Publisher exposes the engine's promotion publisher so work that no request
// drives — the sweeper — can ring the same doorbell the layers do.
func (e *Engine) Publisher() Publisher {
	return e.pub
}
