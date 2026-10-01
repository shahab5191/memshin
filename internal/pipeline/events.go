package pipeline

import (
	"context"

	"github.com/shahab5191/memshin/internal/promotion"
)

type channelPublisher struct {
	ch chan<- promotion.Event
}

// NewChannelPublisher publishes into the in-process dispatcher loop.
func NewChannelPublisher(ch chan<- promotion.Event) promotion.Publisher {
	return &channelPublisher{ch: ch}
}

// Publish never blocks. Layers call this inline on the request path, where a
// blocking send would stall the user's response, and a promotion handler may
// itself publish onward while it is the only thing draining the channel — a
// blocking send there would deadlock the dispatcher against itself.
//
// The context is unused: a non-blocking send has nothing to cancel. Transports
// that do I/O will need it.
func (p *channelPublisher) Publish(_ context.Context, event promotion.Event) error {
	select {
	case p.ch <- event:
		return nil
	default:
		return ErrPromotionQueueFull
	}
}
