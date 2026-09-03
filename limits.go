package acp

import (
	"errors"
	"fmt"
)

// What a connection will hold on a peer's behalf.
//
// A message's size and the time this side will wait were already bounded; its
// count was not, and count is what a peer controls for free. Each of these is a
// place where one connection could grow without limit on nothing but inbound
// messages: a peer that talks faster than the application listens, one that opens
// requests and never lets them finish, and one that names a new session every
// time.
//
// These are not memory proofs. A count bound multiplied by maxMessageBytes is
// still a large number, and claiming otherwise would be claiming more than the
// arithmetic gives. What they remove is the two realistic ways a well-formed peer
// exhausts this process — a backlog that never drains and a population that never
// shrinks — while the size bound handles the single enormous message.
//
// Breaching one ends the connection rather than shedding load. The alternatives
// are worse in ways that are specific rather than aesthetic. Refusing to read
// until the backlog drains turns the documented rule that a notification handler
// must not wait on its own connection into a deadlock, because the response that
// would release it arrives on the read loop that is no longer reading. Dropping
// messages loses protocol state silently. Answering "too busy" invents a code the
// schema does not define. A peer that has passed one of these is either hostile or
// running away from an application that cannot keep up, and neither is a condition
// this side can recover from in place.
const (
	// defaultQueuedDeliveries bounds the messages read but not yet delivered. It is
	// reached only when the delivery loop falls behind the read loop, which for a
	// turn's session/update stream means an application handler slower than the
	// agent producing it.
	defaultQueuedDeliveries = 1024

	// defaultInflightRequests bounds the inbound calls being served at once. Each
	// one holds a goroutine, a context and the right to answer, so this is the bound
	// on work a peer can start and never finish.
	defaultInflightRequests = 1024

	// defaultSessionHandles bounds the session handles one connection caches.
	//
	// A ClientConn reclaims an entry when Close or DeleteSession succeeds. An
	// AgentConn reclaims one only when it serves session/close, because its
	// session/delete handler takes no handle and never names the cache. So a peer
	// that opens a session per prompt and closes none grows this population until
	// it ends the connection.
	defaultSessionHandles = 1024
)

// Limits bounds what one connection will hold on a peer's behalf. The zero value
// takes every default, and a zero field takes the default for that field.
//
// The defaults suit an application whose handlers return promptly. Raising a
// bound is how an application that knows otherwise says so, and the field worth
// raising is almost always QueuedDeliveries: it is the only one an honest peer
// reaches, because a turn's session/update stream is produced by an agent and
// consumed by a handler that may render it.
//
// Because a breach ends the connection, a bound set too low for the application
// that chose it is a self-inflicted disconnection. This is why the field is here
// rather than a constant: the package cannot know how fast a caller's handler
// returns, and guessing on the caller's behalf is what a configuration field
// exists to stop.
type Limits struct {
	// QueuedDeliveries bounds the messages read but not yet delivered, which is
	// how far the delivery loop may fall behind the read loop.
	QueuedDeliveries int

	// InflightRequests bounds the inbound calls being served at once.
	InflightRequests int

	// SessionHandles bounds the session handles one connection caches.
	SessionHandles int
}

// resolve substitutes the default for every field left zero.
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
	return l
}

// check refuses a bound that cannot be served, at construction rather than on the
// message that breaches it. A negative bound is unreachable arithmetic dressed as
// a limit: every push would refuse and the first inbound message would end the
// connection, which is a configuration error and not a peer's fault.
func (l Limits) check() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"QueuedDeliveries", l.QueuedDeliveries},
		{"InflightRequests", l.InflightRequests},
		{"SessionHandles", l.SessionHandles},
	} {
		if bound.value < 0 {
			return fmt.Errorf("acp: Limits.%s is %d; a bound cannot be negative", bound.name, bound.value)
		}
	}
	return nil
}

var (
	errTooManyQueued   = errors.New("acp: inbound delivery queue limit exceeded")
	errTooManyInflight = errors.New("acp: inbound request limit exceeded")
	errTooManySessions = errors.New("acp: session handle limit exceeded")
)
