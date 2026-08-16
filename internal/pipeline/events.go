package pipeline

import "context"

// PromotionEvent is a doorbell, not a delivery. It names the user whose data is
// ready and the layers on either end, and carries no content: the target reads
// the outstanding rows from the source itself.
//
// That keeps the event trivially serialisable, but the reason it matters is
// idempotence. Every redelivery mechanism we are likely to end up with — a
// retry here, a broker's at-least-once guarantee later — can hand the same
// event over twice. A redelivered doorbell re-reads current state and finds
// whatever is still outstanding, which may be nothing. A redelivered payload
// would be written twice.
type PromotionEvent struct {
	UserID      string
	SourceLayer string
	TargetLayer string
}

// Publisher hands a promotion event to whatever transport is in use. Layers
// depend on this rather than on a channel directly, so the in-process
// dispatcher can be replaced by a broker client or an RPC stub without
// touching a single layer signature.
type Publisher interface {
	Publish(ctx context.Context, event PromotionEvent) error
}

type Consumer interface {
	Consume(ctx context.Context) (<-chan PromotionEvent, error)
}

type channelPublisher struct {
	ch chan<- PromotionEvent
}

type channelConsumer struct {
	ch <-chan PromotionEvent
}

// NewChannelPublisher publishes into the in-process dispatcher loop.
func NewChannelPublisher(ch chan<- PromotionEvent) Publisher {
	return &channelPublisher{ch: ch}
}

func NewChannelConsumer(ch <-chan PromotionEvent) *channelConsumer {
	return &channelConsumer{ch: ch}
}

func (p *channelPublisher) Publish(_ context.Context, event PromotionEvent) error {
	select {
	case p.ch <- event:
		return nil
	default:
		return ErrPromotionQueueFull
	}
}

func (c *channelConsumer) Consume(_ context.Context) (<-chan PromotionEvent, error) {
	return c.ch, nil
}
