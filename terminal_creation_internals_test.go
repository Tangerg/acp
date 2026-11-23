package acp

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestTerminalCreationHasOneReleaseOwnerAcrossDeliveryAndAbandonment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream, conn := rawAgentConnection(t)
		defer func() { _ = conn.Close() }()
		if err := stream.Write(t.Context(), rawCall(1, methodInitialize, `{"protocolVersion":1,"clientCapabilities":{"terminal":true}}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		var released atomic.Int32
		go func() {
			for {
				message, err := stream.Read(t.Context())
				if err != nil {
					return
				}
				call, ok := message.(*jsonrpc.Request)
				if !ok || call.Method != methodTerminalRelease {
					t.Errorf("cleanup sent %T, want terminal/release", message)
					return
				}
				released.Add(1)
				if err := stream.Write(t.Context(), &jsonrpc.Response{ID: call.ID, Result: json.RawMessage(`{}`)}); err != nil {
					return
				}
			}
		}()
		session := conn.session("session")
		var expected int32
		for _, order := range []string{"abandon first", "response first", "concurrent"} {
			for range 16 {
				creation := &terminalCreation{session: session}
				accept := func() {
					if err := creation.accept(&jsonrpc.Response{Result: json.RawMessage(`{"terminalId":"terminal"}`)}); err != nil {
						t.Error(err)
					}
				}
				switch order {
				case "abandon first":
					creation.abandon()
					accept()
				case "response first":
					accept()
					creation.abandon()
				case "concurrent":
					start := make(chan struct{})
					var racers sync.WaitGroup
					racers.Go(func() { <-start; accept() })
					racers.Go(func() { <-start; creation.abandon() })
					close(start)
					racers.Wait()
				}
				creation.abandon()
				synctest.Wait()
				expected++
				if got := released.Load(); got != expected {
					t.Fatalf("%s released %d terminals, want %d", order, got, expected)
				}
			}
		}
		creation := &terminalCreation{session: session}
		if err := creation.accept(&jsonrpc.Response{Result: json.RawMessage(`{"terminalId":"taken"}`)}); err != nil {
			t.Fatal(err)
		}
		handle, response := creation.take()
		if handle == nil || response == nil || handle.ID() != response.TerminalID {
			t.Fatal("caller did not receive the created terminal")
		}
		creation.abandon()
		synctest.Wait()
		if released.Load() != expected {
			t.Error("abandonment released a terminal already transferred to its caller")
		}
		if _, err := handle.Release(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if released.Load() != expected+1 {
			t.Error("the caller could not release its terminal")
		}
	})
}
