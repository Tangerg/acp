package acp

import (
	"errors"
	"fmt"
	"sync"
)

var errElicitationIDInUse = errors.New("acp: URL elicitation identifier is already in use")

type urlElicitationState uint8

const (
	// urlElicitationReserved has an ID claimed by create work not yet answered.
	urlElicitationReserved urlElicitationState = iota
	// urlElicitationOutstanding was accepted and awaits an optional completion.
	urlElicitationOutstanding
	// urlElicitationCompleting has a completion being written.
	urlElicitationCompleting
)

// urlElicitations owns the protocol lifetime of every URL elicitation on one
// connection. A record has identity as well as an ID, so a delayed rollback can
// never touch a later reuse of the same wire ID.
//
// Completing an accepted interaction is optional, so the outstanding population
// is bounded by forgetting the oldest outstanding record rather than by refusing
// new elicitations: a forgotten ID only loses the ability to recognise its late
// completion.
type urlElicitations struct {
	mu      sync.Mutex
	entries map[ElicitationID]*urlElicitation
	limit   int
	next    uint64
}

type urlElicitation struct {
	owner *urlElicitations
	id    ElicitationID
	state urlElicitationState
	// order is when the record was created, for forgetting the oldest.
	order uint64
}

type urlElicitationCompletion struct {
	elicitation *urlElicitation
}

func newURLElicitations(limit int) *urlElicitations {
	return &urlElicitations{entries: make(map[ElicitationID]*urlElicitation), limit: limit}
}

// reserve establishes uniqueness before work that may put the ID on the wire or
// in front of a user. The reservation is provisional until an accepting answer.
func (u *urlElicitations) reserve(id ElicitationID) (*urlElicitation, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if _, exists := u.entries[id]; exists {
		return nil, fmt.Errorf("%w: %q", errElicitationIDInUse, id)
	}
	if len(u.entries) >= u.limit && !u.forgetOldestOutstanding() {
		return nil, fmt.Errorf("%w: URL elicitations in progress (limit %d)", ErrLimitExceeded, u.limit)
	}
	u.next++
	record := &urlElicitation{owner: u, id: id, state: urlElicitationReserved, order: u.next}
	u.entries[id] = record
	return record, nil
}

// forgetOldestOutstanding is called under the lock. Reserved and completing
// records are mid-transaction and are never forgotten.
func (u *urlElicitations) forgetOldestOutstanding() bool {
	var oldest *urlElicitation
	for _, record := range u.entries {
		if record.state == urlElicitationOutstanding && (oldest == nil || record.order < oldest.order) {
			oldest = record
		}
	}
	if oldest == nil {
		return false
	}
	delete(u.entries, oldest.id)
	return true
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

// beginCompletion reserves an outstanding record for one completion, so that a
// delayed failed write cannot reopen or delete a newer interaction.
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

// unsent restores the record after a write the transport proved uncommitted.
func (c *urlElicitationCompletion) unsent() {
	record := c.elicitation
	record.owner.mu.Lock()
	defer record.owner.mu.Unlock()
	if record.current(urlElicitationCompleting) {
		record.state = urlElicitationOutstanding
	}
}

// receiveCompletion implements the client's must-ignore rule: only an accepted
// interaction is completed, never a reservation whose handler has not consented.
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

// current is called under the owner's lock.
func (e *urlElicitation) current(state urlElicitationState) bool {
	return e.owner.entries[e.id] == e && e.state == state
}
