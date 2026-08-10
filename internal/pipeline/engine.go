package pipeline

import (
	"context"
	"log/slog"
)

const promotionBuffer = 256

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
	promCh chan PromotionEvent
}

func NewEngine(memory []MemoryLayer, llm LLMProvider) *Engine {
	return &Engine{
		memory: memory,
		llm:    llm,
		promCh: make(chan PromotionEvent, promotionBuffer),
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
		if err := layer.HandlePromotion(ctx, event.UserID, event.Payload, e.promCh); err != nil {
			slog.Error("promotion handler failed",
				"layer", layer.Name(), "user", event.UserID, "error", err)
		}
		return
	}

	slog.Warn("promotion event addressed to unknown layer",
		"target", event.TargetLayer, "source", event.Payload.SourceLayer, "user", event.UserID)
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
