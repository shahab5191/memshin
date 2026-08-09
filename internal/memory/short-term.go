package memory

import (
	"sync"

	"github.com/shahab5191/memshin/internal/pipeline"
)

const (
	ShortTermMemoryTag = "short-term"
)

type Conversation struct {
	Role      string
	Content   string
	Timestamp int64
}

type UserSession struct {
	mu             sync.Mutex
	RecentMessages []Conversation // Max 8 recent messages (4 before-and-forth pairs)
	// It can outgrow the limit until previous messages are promoted to mid-term
	Summeries []string // Max 4 summaries
}

func (us *UserSession) JoinRecentMessages() string {
	us.mu.Lock()
	defer us.mu.Unlock()

	joined := ""
	for _, conv := range us.RecentMessages {
		joined += conv.Role + ": " + conv.Content + "\n"
	}
	return joined
}

type ShortTermMemory struct {
	sessions map[string]*UserSession
	mu       sync.Mutex
}

func NewShortTermMemory() *ShortTermMemory {
	return &ShortTermMemory{
		sessions: make(map[string]*UserSession),
	}
}

func (stm *ShortTermMemory) Name() string {
	return "ShortTermMemory"
}

func (stm *ShortTermMemory) RequestProcess(ctx *pipeline.ChatContext) error {
	stm.mu.Lock()
	defer stm.mu.Unlock()

	session, exists := stm.sessions[ctx.UserID]
	if !exists {
		session = &UserSession{
			RecentMessages: make([]Conversation, 0, 8),
			Summeries:      make([]string, 0, 4),
		}
		stm.sessions[ctx.UserID] = session
	}

	session.mu.Lock()
	session.RecentMessages = append(session.RecentMessages, Conversation{
		Role:    "user",
		Content: ctx.OriginalPrompt,
	})
	session.mu.Unlock()

	// In request processing, we do not check for the limit of recent messages. We only check in response processing.
	// Add recent messages to the context
	joinedMessages := session.JoinRecentMessages()
	ctx.AddBlock(pipeline.ContextBlock{
		Source:   stm.Name(),
		Tag:      ShortTermMemoryTag,
		Content:  joinedMessages,
		Priority: 1,
	})

	return nil
}

func (stm *ShortTermMemory) ResponseProcess(
	ctx *pipeline.ChatContext,
	llmResponse string,
	promCh chan<- pipeline.PromotionEvent,
) error {
	stm.mu.Lock()
	defer stm.mu.Unlock()

	session, exists := stm.sessions[ctx.UserID]
	if !exists {
		return nil // No session found, nothing to process
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Add the LLM response to recent messages
	session.RecentMessages = append(session.RecentMessages, Conversation{
		Role:    "assistant",
		Content: llmResponse,
	})

	// Check if we need to promote messages to mid-term memory
	go stm.PublishExtraMemory(promCh, session, ctx.UserID)

	return nil
}

func (stm *ShortTermMemory) HandlePromotion(
	userId string,
	payload string,
	promCh chan<- pipeline.PromotionEvent,
) error {
	// For short-term memory, we don't handle promotions from other layers.
	// This method is a no-op for this layer.
	return nil
}

func (stm *ShortTermMemory) PublishExtraMemory(promCh chan<- pipeline.PromotionEvent, session *UserSession, userId string) {
	stm.mu.Lock()
	defer stm.mu.Unlock()

	session.mu.Lock()
	defer session.mu.Unlock()
	// Check if we need to promote messages to mid-term memory
	if len(session.RecentMessages) > 8 {
		// Promote the oldest messages to mid-term memory
		// For now do nothing, just send a promotion event

		content := make([]pipeline.ContextBlock, 0, len(session.RecentMessages)-8)
		for i := 0; i < len(session.RecentMessages)-8; i++ {
			content = append(content, pipeline.ContextBlock{
				Source:   stm.Name(),
				Tag:      ShortTermMemoryTag,
				Content:  session.RecentMessages[i].Role + ": " + session.RecentMessages[i].Content,
				Priority: 1,
			})
		}

		// we keep the message until ack from mid-term memory, so we don't remove them from recent messages yet

		promCh <- pipeline.PromotionEvent{
			UserID: userId,
			Payload: pipeline.PromotionPayload{
				SourceLayer: ShortTermMemoryTag,
				Content:     content,
			},
		}
	}
}
