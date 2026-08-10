package pipeline

import (
	"context"
	"log/slog"
)

type MemoryLayer interface {
	Name() string
	RequestProcess(ctx context.Context, chat *ChatContext) error
	ResponseProcess(ctx context.Context, chat *ChatContext, llmResponse string, promCh chan<- PromotionEvent) error
	HandlePromotion(ctx context.Context, userID string, payload PromotionPayload, promCh chan<- PromotionEvent) error
}

type LLMProvider interface {
	Name() string
	GenerateResponse(ctx context.Context, chat *ChatContext) (string, error)
}

type Engine struct {
	memory []MemoryLayer
	llm    LLMProvider
	promCh chan<- PromotionEvent
}

// NewEngine wires the layers together. promCh may be nil only while no layer
// publishes promotions — a send on a nil channel blocks forever, so a layer
// that publishes with no consumer attached leaks a goroutine permanently.
func NewEngine(memory []MemoryLayer, llm LLMProvider, promCh chan<- PromotionEvent) *Engine {
	return &Engine{
		memory: memory,
		llm:    llm,
		promCh: promCh,
	}
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
		if err := layer.RequestProcess(ctx, chat); err != nil {
			return "", err
		}
	}

	// step 2: generate the response
	r, err := e.llm.GenerateResponse(ctx, chat)
	if err != nil {
		return "", err
	}

	// step 3: let the layers record the turn.
	for _, layer := range e.memory {
		if err := layer.ResponseProcess(ctx, chat, r, e.promCh); err != nil {
			slog.Error("memory layer failed to record turn",
				"layer", layer.Name(), "user", userID, "error", err)
		}
	}

	return r, nil
}
