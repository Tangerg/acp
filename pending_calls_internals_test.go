package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestCancelledTerminalCreatesRemainBoundedUntilTheirResponsesArrive(t *testing.T) {
	const bound = 2
	agent := blockingAgent(t, Limits{PendingCalls: bound}, make(chan struct{}), make(chan struct{}))
	stream, conn := connectRawAgent(t, agent)
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := stream.Write(ctx, rawCall(1, methodInitialize, `{"protocolVersion":1,"clientCapabilities":{"terminal":true}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatal(err)
	}
	session := conn.session("session")
	var creates []*jsonrpc.Request
	for range bound {
		createCtx, cancelCreate := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() {
			_, _, err := session.CreateTerminal(createCtx, &CreateTerminalParams{Command: "/bin/true"})
			result <- err
		}()
		message, err := stream.Read(ctx)
		if err != nil {
			cancelCreate()
			t.Fatal(err)
		}
		request, ok := message.(*jsonrpc.Request)
		if !ok || request.Method != methodTerminalCreate {
			cancelCreate()
			t.Fatalf("read %T, want terminal/create", message)
		}
		creates = append(creates, request)
		cancelCreate()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled create returned %v", err)
		}
		message, err = stream.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cancellation, ok := message.(*jsonrpc.Request); !ok || cancellation.Method != methodCancelRequest {
			t.Fatalf("read %T, want cancellation", message)
		}
	}
	if _, _, err := session.CreateTerminal(ctx, &CreateTerminalParams{Command: "/bin/true"}); !errors.Is(err, ErrTooManyPendingCalls) {
		t.Fatalf("create past quota returned %v", err)
	}
	conn.calls.mu.Lock()
	retained := len(conn.calls.waiting)
	conn.calls.mu.Unlock()
	if retained != bound {
		t.Errorf("retained %d calls, want bounded pending creates %d", retained, bound)
	}
	if conn.ended() {
		t.Fatal("outbound backpressure closed the connection")
	}

	if err := stream.Write(ctx, &jsonrpc.Response{ID: creates[0].ID, Result: json.RawMessage(`{"terminalId":"late"}`)}); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release, ok := message.(*jsonrpc.Request)
	if !ok || release.Method != methodTerminalRelease {
		t.Fatalf("late creation sent %T, want terminal/release", message)
	}
	if err := stream.Write(ctx, &jsonrpc.Response{ID: release.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := conn.calls.awaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}

	probe := make(chan error, 1)
	go func() { probe <- conn.Call(ctx, "_test/after-release", nil, nil) }()
	message, err = stream.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok || request.Method != "_test/after-release" {
		t.Fatalf("read %T, want admitted probe", message)
	}
	if err := stream.Write(ctx, &jsonrpc.Response{ID: request.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-probe; err != nil {
		t.Fatalf("response failed to free outbound capacity: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Wait(); err != nil {
		t.Fatal(err)
	}
	conn.calls.mu.Lock()
	retained = len(conn.calls.waiting)
	conn.calls.mu.Unlock()
	if retained != 0 {
		t.Error("connection end retained pending calls")
	}
}

func TestAbandonedTerminalCleanupWaitsForLocallyOccupiedCallCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent := blockingAgent(t, Limits{PendingCalls: 1}, make(chan struct{}), make(chan struct{}))
		stream, conn := connectRawAgent(t, agent)
		defer func() { _ = conn.Close() }()
		if err := stream.Write(t.Context(), rawCall(1, methodInitialize, `{"protocolVersion":1,"clientCapabilities":{"terminal":true}}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		probeResult := make(chan error, 1)
		go func() { probeResult <- conn.Call(t.Context(), "_test/hold-capacity", nil, nil) }()
		message, err := stream.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		probe, ok := message.(*jsonrpc.Request)
		if !ok {
			t.Fatalf("read %T, want a pending request", message)
		}

		// Another call can claim the slot between a creation reply and the
		// cleanup task starting. A no-write quota refusal must remain retryable.
		creation := &terminalCreation{session: conn.session("session")}
		creation.abandon()
		if err := creation.accept(&jsonrpc.Response{Result: json.RawMessage(`{"terminalId":"late"}`)}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := stream.Write(t.Context(), &jsonrpc.Response{ID: probe.ID, Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := <-probeResult; err != nil {
			t.Fatal(err)
		}
		message, err = stream.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release, ok := message.(*jsonrpc.Request)
		if !ok || release.Method != methodTerminalRelease {
			t.Fatalf("read %T, want retried terminal/release", message)
		}
		if err := stream.Write(t.Context(), &jsonrpc.Response{ID: release.ID, Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		conn.calls.mu.Lock()
		remaining := len(conn.calls.waiting)
		conn.calls.mu.Unlock()
		if remaining != 0 {
			t.Errorf("completed cleanup retained %d pending calls", remaining)
		}
	})
}

func TestPendingCallAdmissionReleasesUnsentAndCompletedReservations(t *testing.T) {
	pending := newCalls(1)
	first, err := pending.begin(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.begin(nil, nil); !errors.Is(err, ErrTooManyPendingCalls) {
		t.Fatalf("full admission returned %v", err)
	}
	pending.retire(first.id)
	second, err := pending.begin(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending.deliver(&jsonrpc.Response{ID: second.id})
	if _, err := pending.begin(nil, nil); err != nil {
		t.Fatal(err)
	}
	pending.close()
	if _, err := pending.begin(nil, nil); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("closed admission returned %v", err)
	}
	if err := (Limits{PendingCalls: -1}).check(); err == nil {
		t.Fatal("negative outbound bound was accepted")
	}
}
