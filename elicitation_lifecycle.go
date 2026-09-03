package acp

import (
	"errors"
	"fmt"
	"sync"
)

var errElicitationIDInUse = errors.New("acp: URL elicitation identifier is already in use")

type urlElicitationState uint8

const (
	urlElicitationReserved urlElicitationState = iota
	urlElicitationOutstanding
	urlElicitationCompleting
)

// urlElicitations owns the protocol lifetime of every URL elicitation on one
// connection. A record has identity as well as an ID so a delayed rollback from
// an earlier transaction can never remove a later reuse of the same wire ID.
type urlElicitations struct {
	mu      sync.Mutex
	entries map[ElicitationID]*urlElicitation
	limit   int
}

type urlElicitation struct {
	owner *urlElicitations
	id    ElicitationID
	state urlElicitationState
}

type urlElicitationCompletion struct {
	elicitation *urlElicitation
}

// reserve establishes uniqueness before work that may put the ID on the wire or
// in front of a user. It is provisional until the create response accepts the
// URL interaction: decline, cancellation, and failure reject the reservation.
func (u *urlElicitations) reserve(id ElicitationID) (*urlElicitation, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if _, exists := u.entries[id]; exists {
		return nil, fmt.Errorf("%w: %q", errElicitationIDInUse, id)
	}
	if len(u.entries) >= u.limit {
		return nil, fmt.Errorf("%w (limit %d)", errTooManyElicitations, u.limit)
	}
	if u.entries == nil {
		u.entries = make(map[ElicitationID]*urlElicitation)
	}
	record := &urlElicitation{owner: u, id: id, state: urlElicitationReserved}
	u.entries[id] = record
	return record, nil
}

func (e *urlElicitation) accept() {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.current(urlElicitationReserved) {
		e.state = urlElicitationOutstanding
	}
}

func (e *urlElicitation) reject() {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.current(urlElicitationReserved) {
		delete(e.owner.entries, e.id)
	}
}

// beginCompletion reserves the outstanding record for one notification. The ID
// cannot be reused while the write is in flight, which prevents a delayed failed
// completion from reopening or deleting a newer interaction under the same ID.
func (u *urlElicitations) beginCompletion(id ElicitationID) (*urlElicitationCompletion, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	record, exists := u.entries[id]
	if !exists || record.state != urlElicitationOutstanding {
		return nil, false
	}
	record.state = urlElicitationCompleting
	return &urlElicitationCompletion{elicitation: record}, true
}

func (c *urlElicitationCompletion) sent() {
	record := c.elicitation
	record.owner.mu.Lock()
	defer record.owner.mu.Unlock()
	if record.current(urlElicitationCompleting) {
		delete(record.owner.entries, record.id)
	}
}

func (c *urlElicitationCompletion) unsent() {
	record := c.elicitation
	record.owner.mu.Lock()
	defer record.owner.mu.Unlock()
	if record.current(urlElicitationCompleting) {
		record.state = urlElicitationOutstanding
	}
}

// receiveCompletion implements the client's MUST-ignore rule. Only an accepted
// URL interaction is outstanding; a reservation whose handler has not consented
// yet is deliberately not completed by an early or otherwise invalid notice.
func (u *urlElicitations) receiveCompletion(id ElicitationID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	record, exists := u.entries[id]
	if !exists || record.state != urlElicitationOutstanding {
		return false
	}
	delete(u.entries, id)
	return true
}

func (e *urlElicitation) current(state urlElicitationState) bool {
	return e.owner.entries[e.id] == e && e.state == state
}
