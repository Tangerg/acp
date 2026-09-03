package acp

import (
	"errors"
	"fmt"
)

// A message's size and the time this side will wait were already bounded; its
// count was not, and count is what a peer controls for free. These are the four
// places one connection could grow without limit on nothing but inbound messages.
//
// They are not memory proofs — a count times maxMessageBytes is still a large
// number. What they remove is the two realistic ways a well-formed peer exhausts
// this process: a backlog that never drains and a population that never shrinks.
//
// Breaching one ends the connection rather than shedding load, and the
// alternatives fail specifically rather than merely being uglier. Refusing to read
// until the backlog drains turns the documented rule that a notification handler
// must not wait on its own connection into a deadlock, because the response that
// would release it arrives on the read loop that is no longer reading. Dropping
// messages loses protocol state silently. Answering "too busy" invents a code the
// schema does not define.
const (
	defaultQueuedDeliveries = 1024

	// Each inbound call holds a goroutine, a context and the right to answer, so
	// this is the bound on work a peer can start and never finish.
	defaultInflightRequests = 1024

	// The population shrinks unevenly: a ClientConn reclaims an entry when Close or
	// DeleteSession succeeds, but an AgentConn only when it serves session/close,
	// because its session/delete handler takes no handle and never names the cache.
	// So a peer opening a session per prompt and closing none grows this until the
	// connection ends.
	defaultSessionHandles = 1024

	// A URL elicitation stays outstanding until a completion clears it, and the
	// protocol makes sending one optional — so a peer that opens pages and
	// completes none grows this set for as long as the connection lives.
	defaultOutstandingElicitations = 1024
)

// Limits bounds what one connection will hold on a peer's behalf. The zero value
// takes every default, as does any field left zero.
//
// QueuedDeliveries is almost always the one worth raising: it is the only bound an
// honest peer reaches, because a turn's session/update stream is produced by an
// agent and consumed by a handler that may render it. Since a breach ends the
// connection, a bound too low for the application that chose it is a self-inflicted
// disconnection — which is why this is a field and not a constant. The package
// cannot know how fast a caller's handler returns.
type Limits struct {
	QueuedDeliveries        int
	InflightRequests        int
	SessionHandles          int
	OutstandingElicitations int
}

func (l Limits) resolve() Limits {
	if l.QueuedDeliveries == 0 {
		l.QueuedDeliveries = defaultQueuedDeliveries
	}
	if l.InflightRequests == 0 {
		l.InflightRequests = defaultInflightRequests
	}
	if l.SessionHandles == 0 {
		l.SessionHandles = defaultSessionHandles
	}
	if l.OutstandingElicitations == 0 {
		l.OutstandingElicitations = defaultOutstandingElicitations
	}
	return l
}

// A negative bound is unreachable arithmetic dressed as a limit: every push would
// refuse and the first inbound message would end the connection, blaming a peer
// for a configuration error. Refused where it is stated instead.
func (l Limits) check() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"QueuedDeliveries", l.QueuedDeliveries},
		{"InflightRequests", l.InflightRequests},
		{"SessionHandles", l.SessionHandles},
		{"OutstandingElicitations", l.OutstandingElicitations},
	} {
		if bound.value < 0 {
			return fmt.Errorf("acp: Limits.%s is %d; a bound cannot be negative", bound.name, bound.value)
		}
	}
	return nil
}

var (
	errTooManyQueued       = errors.New("acp: inbound delivery queue limit exceeded")
	errTooManyInflight     = errors.New("acp: inbound request limit exceeded")
	errTooManySessions     = errors.New("acp: session handle limit exceeded")
	errTooManyElicitations = errors.New("acp: outstanding URL elicitation limit exceeded")
)
