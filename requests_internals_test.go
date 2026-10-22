package acp

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

func TestCancellationAppliesToResponseEncodingFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		cancelled bool
		result    *PromptResponse
		wantCode  ErrorCode
	}{
		{"cancelled malformed result", true, &PromptResponse{}, ErrorCodeRequestCancelled},
		{"ordinary malformed result", false, &PromptResponse{}, ErrorCodeInternalError},
		{"cancelled valid result", true, &PromptResponse{StopReason: StopReasonEndTurn}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := newTestAgentConn()
			stream := &observedResponseStream{
				observed: make(chan jsonrpc.Message, 1), resume: make(chan struct{}),
			}
			close(stream.resume)
			conn.transport = stream
			call, err := conn.requests.accept(t.Context(), jsonrpc2.Int64ID(1))
			if err != nil {
				t.Fatal(err)
			}
			defer call.finish()
			if test.cancelled {
				conn.requests.cancel(call.id)
			}
			if !conn.respond(call, test.result, nil) {
				t.Fatal("the response was not written")
			}
			response := (<-stream.observed).(*jsonrpc.Response)
			if test.wantCode != 0 {
				if response.Error == nil || response.Error.Code != int64(test.wantCode) {
					t.Fatalf("response error = %v, want %d", response.Error, test.wantCode)
				}
				return
			}
			if response.Error != nil {
				t.Fatalf("a valid result was replaced by %v", response.Error)
			}
			var result PromptResponse
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.StopReason != StopReasonEndTurn {
				t.Fatalf("stop reason = %s, want the handler's valid result", result.StopReason)
			}
		})
	}
}

func TestAnAutomaticResponseRetainsItsRequestUntilTheWriteSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newTestAgentConn()
		barrier := &responseBarrier{entered: make(chan struct{}), release: make(chan struct{})}
		conn.transport = barrier
		conn.turns.claim("sess-1", &inboundRequest{id: jsonrpc2.Int64ID(1)})
		request := rawCall(2, methodSessionPrompt, promptParams)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		call, err := conn.requests.accept(ctx, request.ID)
		if err != nil {
			t.Fatal(err)
		}
		conn.deliverRequest(ctx, request, call)
		<-barrier.entered
		synctest.Wait()
		select {
		case <-call.settled:
			t.Error("the automatic response released its request before writing it")
		default:
		}
		close(barrier.release)
		conn.life.work.Wait()
	})
}

func TestAnAnsweredRequestRetainsOnlyItsOwnHandlerWork(t *testing.T) {
	requests := newRequests(2)
	id := jsonrpc2.Int64ID(1)
	first, err := requests.accept(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed := first.claim(); !claimed {
		t.Fatal("first answer was not claimed")
	}
	first.settle()
	second, err := requests.accept(t.Context(), id)
	if err != nil {
		t.Fatalf("an answered ID could not be reused while its handler stopped: %v", err)
	}
	if _, err := requests.accept(t.Context(), jsonrpc2.Int64ID(2)); err == nil {
		t.Error("an unfinished handler no longer counted against the in-flight bound")
	}
	if _, claimed := first.claim(); claimed {
		t.Error("the old handler claimed a second answer")
	}
	first.finish()
	if requests.serving[id] != second {
		t.Fatal("the old handler removed the newer request")
	}
	if _, claimed := second.claim(); !claimed {
		t.Fatal("the old handler took the newer request's answer")
	}
	second.settle()
	second.finish()
	if requests.inflight != 0 {
		t.Fatalf("finished work retained %d in-flight requests", requests.inflight)
	}
}

func TestAReceivedResponseMakesItsWireIDReusableBeforeWriteReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newTestAgentConn()
		stream := &observedResponseStream{
			observed: make(chan jsonrpc.Message), resume: make(chan struct{}),
		}
		conn.transport = stream
		id := jsonrpc2.Int64ID(1)
		first, err := conn.requests.accept(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		written := make(chan bool, 1)
		go func() {
			defer first.finish()
			written <- conn.respond(first, struct{}{}, nil)
		}()
		response := (<-stream.observed).(*jsonrpc.Response)
		if response.ID != id {
			t.Errorf("response used %v, want %v", response.ID, id)
		}
		second, reuseErr := conn.requests.accept(t.Context(), id)
		if err := first.beginSending(t.Context()); err == nil {
			first.finishSending()
			t.Error("the answered request could send an elicitation under its successor's ID")
		}
		close(stream.resume)
		if !<-written {
			t.Error("first response was not written")
		}
		synctest.Wait()
		if reuseErr != nil {
			t.Fatalf("the peer had its response, but reusing its ID failed: %v", reuseErr)
		}
		if conn.requests.serving[id] != second {
			t.Fatal("the old write removed its successor")
		}
		if _, claimed := first.claim(); claimed {
			t.Fatal("the old handler claimed the successor's response")
		}
		if _, claimed := second.claim(); !claimed {
			t.Fatal("the successor lost its response")
		}
		second.settle()
		second.finish()
		if conn.requests.inflight != 0 {
			t.Fatalf("settled requests retained %d obligations", conn.requests.inflight)
		}
	})
}

type observedResponseStream struct {
	observed chan jsonrpc.Message
	resume   chan struct{}
}

func (*observedResponseStream) Read(ctx context.Context) (jsonrpc.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *observedResponseStream) Write(_ context.Context, message jsonrpc.Message) error {
	s.observed <- message
	<-s.resume
	return nil
}

func (*observedResponseStream) Close() error { return nil }

func TestConcurrentTurnCancellationsRetainTheSamePermissionObligations(t *testing.T) {
	var turns clientTurns
	turn, _ := turns.begin("session")
	permission := &inboundRequest{id: jsonrpc2.Int64ID(1)}
	turns.registerPermission("session", permission)
	first := turns.beginCancellation("session")
	second := turns.beginCancellation("session")
	for _, cancellation := range []clientCancellation{first, second} {
		if len(cancellation.pending) != 1 || cancellation.pending[0] != permission {
			t.Error("a concurrent cancellation could overtake the outstanding permission answer")
		}
		cancellation.finish()
	}
	turn.complete()
}

func TestCompletedClientTurnsReleaseTheirRecordsWithoutAffectingSuccessors(t *testing.T) {
	var turns clientTurns
	first, _ := turns.begin("session")
	first.complete()
	if len(turns.sessions) != 0 {
		t.Fatal("a completed turn was retained by the connection")
	}
	second, _ := turns.begin("session")
	first.complete()
	if _, started := turns.begin("session"); started {
		t.Error("a delayed completion released the next turn")
	}
	cancellation := turns.beginCancellation("session")
	second.complete()
	if _, started := turns.begin("session"); started {
		t.Error("a pending cancellation did not retain the session")
	}
	cancellation.finish()
	if len(turns.sessions) != 0 {
		t.Fatal("settled cancellation retained a completed turn")
	}
}

type responseBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *responseBarrier) Read(ctx context.Context) (jsonrpc.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *responseBarrier) Write(context.Context, jsonrpc.Message) error {
	close(b.entered)
	<-b.release
	return nil
}

func (b *responseBarrier) Close() error { return nil }
