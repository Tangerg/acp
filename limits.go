package acp

import "errors"

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
	// maxQueuedDeliveries bounds the messages read but not yet delivered. It is
	// reached only when the delivery loop falls behind the read loop, which for a
	// turn's session/update stream means an application handler slower than the
	// agent producing it.
	maxQueuedDeliveries = 1024

	// maxInflightRequests bounds the inbound calls being served at once. Each one
	// holds a goroutine, a context and the right to answer, so this is the bound
	// on work a peer can start and never finish.
	maxInflightRequests = 1024

	// maxSessionsPerConnection bounds the session handles one connection caches.
	//
	// A ClientConn reclaims an entry when Close or DeleteSession succeeds. An
	// AgentConn reclaims one only when it serves session/close, because its
	// session/delete handler takes no handle and never names the cache. So a peer
	// that opens a session per prompt and closes none grows this population until
	// it ends the connection.
	maxSessionsPerConnection = 1024
)

var (
	errTooManyQueued   = errors.New("acp: inbound delivery queue limit exceeded")
	errTooManyInflight = errors.New("acp: inbound request limit exceeded")
	errTooManySessions = errors.New("acp: session handle limit exceeded")
)
