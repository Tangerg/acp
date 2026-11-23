package acp

import (
	"errors"
	"fmt"
)

// Default bounds on the connection-owned populations a peer can grow without an
// application deliberately retaining each member. They bound counts, not memory:
// one legal message can still be large.
const (
	defaultQueuedDeliveries        = 1024
	defaultInflightRequests        = 1024
	defaultPendingCalls            = 1024
	defaultOutstandingElicitations = 1024
)

// Limits bounds the protocol state one connection holds. A zero field takes its
// default of 1024; a negative one is refused at construction.
//
// Breaching QueuedDeliveries or InflightRequests ends the connection, because
// refusing to read would deadlock a notification handler waiting on a response
// behind it. QueuedDeliveries is usually the one worth raising: a turn's
// session/update stream is consumed by a handler that may render it, and only the
// application knows how slow that handler may be. Breaching PendingCalls refuses
// the new call with [ErrLimitExceeded] and leaves the connection usable.
// OutstandingElicitations bounds URL interactions awaiting completion; at the
// bound the oldest accepted interaction is forgotten, because the protocol makes
// completing one optional.
type Limits struct {
	QueuedDeliveries        int
	InflightRequests        int
	PendingCalls            int
	OutstandingElicitations int
}

func (l Limits) resolve() Limits {
	for _, bound := range []struct {
		value    *int
		fallback int
	}{
		{&l.QueuedDeliveries, defaultQueuedDeliveries},
		{&l.InflightRequests, defaultInflightRequests},
		{&l.PendingCalls, defaultPendingCalls},
		{&l.OutstandingElicitations, defaultOutstandingElicitations},
	} {
		if *bound.value == 0 {
			*bound.value = bound.fallback
		}
	}
	return l
}

func (l Limits) check() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"QueuedDeliveries", l.QueuedDeliveries},
		{"InflightRequests", l.InflightRequests},
		{"PendingCalls", l.PendingCalls},
		{"OutstandingElicitations", l.OutstandingElicitations},
	} {
		if bound.value < 0 {
			return fmt.Errorf("acp: Limits.%s is %d; a bound cannot be negative", bound.name, bound.value)
		}
	}
	return nil
}

// ErrLimitExceeded reports that a [Limits] bound was reached. The wrapped detail
// names which one.
var ErrLimitExceeded = errors.New("acp: limit exceeded")

var (
	errTooManyQueued       = fmt.Errorf("%w: queued deliveries", ErrLimitExceeded)
	errTooManyInflight     = fmt.Errorf("%w: inflight requests", ErrLimitExceeded)
	errTooManyPendingCalls = fmt.Errorf("%w: pending calls", ErrLimitExceeded)
)

func limitError(sentinel error, limit int) error {
	return fmt.Errorf("%w (limit %d)", sentinel, limit)
}
