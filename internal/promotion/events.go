package promotion

import "context"

// Event is a doorbell, not a delivery. It names the user whose data is ready
// and the layers on either end, and carries no content: the target reads the
// outstanding rows from the source itself.
type Event struct {
	UserID      string
	SourceLayer string
	TargetLayer string
}

// Publisher hands a promotion event to whatever transport is in use.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}
