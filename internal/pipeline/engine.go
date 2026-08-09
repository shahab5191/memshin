package pipeline

type MemoryLayer interface {
	Name() string
	RequestProcess(ctx *ChatContext) error
	ResponseProcess(ctx *ChatContext, llmResponse string, promCh chan<- PromotionEvent) error
	HandlePromotion(userId string, payload string, promCh chan<- PromotionEvent) error
}

type LLMProvider interface {
	Name() string
	GenerateResponse(ctx *ChatContext) (string, error)
}

type Engine struct {
	memory []MemoryLayer
	llm    LLMProvider
}

func NewEngine(memory []MemoryLayer, llm LLMProvider) *Engine {
	return &Engine{
		memory: memory,
		llm:    llm,
	}
}

func (e *Engine) Process(userId string, prompt string, sysMsg string) (string, error) {
	ctx := &ChatContext{
		UserID:         userId,
		OriginalPrompt: prompt,
		SystemMessage:  sysMsg,
		Blocks:         []ContextBlock{},
	}
	// step 1: process request through memory layers
	for _, layer := range e.memory {
		if err := layer.RequestProcess(ctx); err != nil {
			return "", err
		}
	}

	// step 2: generate response from LLM
	r, err := e.llm.GenerateResponse(ctx)
	if err != nil {
		return "", err
	}

	// step 3: process response through memory layers
	for _, layer := range e.memory {
		go layer.ResponseProcess(ctx, r, nil);
	}

	return r, nil
}
