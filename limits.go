package acp

import (
	"errors"
	"fmt"
)

// A message's size and the time this side will wait were already bounded; its
// count was not. These are the four connection-owned populations that can grow
// without an application deliberately retaining each member.
//
// They are not memory proofs — a count times maxMessageBytes is still a large
// number. What they remove is the two realistic ways a well-formed peer exhausts
// this process: a backlog that never drains and a population that never shrinks.
//
// The peer drives the delivery, request, and session populations, so breaching
// those ends the connection rather than silently dropping protocol state. URL
// elicitation reservations are different: either peer may originate one, and its
// creation can be refused before a new interaction exists, so that breach fails
// the operation without killing an otherwise healthy connection.
const (
	defaultQueuedDeliveries = 1024

	// Each inbound call holds a goroutine, a context and the right to answer, so
	// this is the bound on work a peer can start and never finish.
	defaultInflightRequests = 1024

	// Successful session/close and session/delete reclaim the named handle on both
	// sides. A peer that names new sessions and closes neither can otherwise grow
	// this population until the connection ends.
	defaultSessionHandles = 1024

	// A URL elicitation stays outstanding after its create response accepts it and
	// until a completion clears it. Reservations and completion writes count too,
	// so concurrent work cannot step around the bound between stable states.
	defaultOutstandingElicitations = 1024
)

// Limits bounds the protocol state one connection will hold. The zero value takes
// every default, as does any field left zero.
//
// QueuedDeliveries is usually the one worth raising: a turn's session/update
// stream is produced by an agent and consumed by a handler that may render it.
// Since that breach ends the connection, a bound too low for the application is a
// self-inflicted disconnection; the package cannot know how fast its handler runs.
type Limits struct {
	QueuedDeliveries int
	InflightRequests int
	SessionHandles   int
	// OutstandingElicitations bounds URL interactions in provisional, accepted,
	// and completing states. Reaching it refuses the new elicitation only.
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
	errTooManyElicitations = errors.New("acp: URL elicitation limit exceeded")
)
