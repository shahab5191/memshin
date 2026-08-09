package pipeline

type PromotionPayload struct {
	SourceLayer string
	Content     []ContextBlock
}

type PromotionEvent struct {
	UserID      string
	TargetLayer string
	Payload     PromotionPayload
}
