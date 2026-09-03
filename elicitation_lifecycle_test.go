package acp

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestURLLifecycleCommitsOnlyAcceptedElicitations(t *testing.T) {
	var elicitations urlElicitations
	elicitations.limit = 1

	declined, err := elicitations.reserve("same")
	if err != nil {
		t.Fatalf("reserve declined elicitation: %v", err)
	}
	if _, duplicateErr := elicitations.reserve("same"); !errors.Is(duplicateErr, errElicitationIDInUse) {
		t.Fatalf("duplicate reservation returned %v, want errElicitationIDInUse", duplicateErr)
	}
	if _, limitErr := elicitations.reserve("other"); !errors.Is(limitErr, errTooManyElicitations) {
		t.Fatalf("reservation past the bound returned %v, want errTooManyElicitations", limitErr)
	}
	if _, outstanding := elicitations.beginCompletion("same"); outstanding {
		t.Fatal("a provisional elicitation was treated as accepted")
	}
	declined.reject()

	accepted, err := elicitations.reserve("same")
	if err != nil {
		t.Fatalf("the declined elicitation did not release its ID: %v", err)
	}
	accepted.accept()
	completion, outstanding := elicitations.beginCompletion("same")
	if !outstanding {
		t.Fatal("the accepted elicitation was not outstanding")
	}
	if _, concurrent := elicitations.beginCompletion("same"); concurrent {
		t.Fatal("two completions claimed the same elicitation")
	}

	completion.unsent()
	retry, outstanding := elicitations.beginCompletion("same")
	if !outstanding {
		t.Fatal("a completion known not to have been sent could not be retried")
	}
	retry.sent()
	if _, err := elicitations.reserve("same"); err != nil {
		t.Fatalf("a completed elicitation did not release its ID: %v", err)
	}
}

func TestCallerCancellationStillObservesAStateOwningResponse(t *testing.T) {
	var elicitations urlElicitations
	elicitations.limit = 1
	reservation, err := elicitations.reserve("late")
	if err != nil {
		t.Fatal(err)
	}

	life := newLifetime()
	// The test exercises await without running a connection. Prevent its remote
	// cancellation side job from being admitted; that job is independent of the
	// waiter-retention invariant under test.
	life.mu.Lock()
	life.stopped = true
	life.mu.Unlock()
	l := &link{life: life, calls: newCalls()}
	call, open := l.calls.begin(func(*jsonrpc.Response) error {
		reservation.accept()
		return nil
	}, reservation.reject)
	if !open {
		t.Fatal("begin call")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.await(ctx, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("await returned %v, want context cancellation", err)
	}
	l.calls.deliver(&jsonrpc.Response{ID: call.id})
	if _, outstanding := elicitations.beginCompletion("late"); !outstanding {
		t.Fatal("the caller retired a response that owned the elicitation's final state")
	}
}

func TestAStaleCompletionCannotChangeAReusedID(t *testing.T) {
	var elicitations urlElicitations
	elicitations.limit = 1

	first, err := elicitations.reserve("same")
	if err != nil {
		t.Fatal(err)
	}
	first.accept()
	stale, ok := elicitations.beginCompletion("same")
	if !ok {
		t.Fatal("begin first completion")
	}
	stale.sent()

	second, err := elicitations.reserve("same")
	if err != nil {
		t.Fatal(err)
	}
	second.accept()
	stale.unsent()

	if !elicitations.receiveCompletion("same") {
		t.Fatal("the stale transaction changed the newer elicitation")
	}
}
