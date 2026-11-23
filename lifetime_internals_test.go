package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

// A caller's deadline does not interrupt a write that has begun: cancellation
// cannot prove which prefix reached the peer, so the write finishes or the
// connection ends, and only Close ends it.
func TestACallersDeadlineDoesNotInterruptAWriteInProgress(t *testing.T) {
	for name, operation := range map[string]func(context.Context, *AgentConn) error{
		"call": func(ctx context.Context, conn *AgentConn) error {
			return conn.Call(ctx, "_test/write", nil, nil)
		},
		"notification": func(ctx context.Context, conn *AgentConn) error {
			return conn.Notify(ctx, "_test/write", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := newTestAgentConn(t)
				reader, peerWriter := io.Pipe()
				peerReader, writer := io.Pipe()
				t.Cleanup(func() { _ = errors.Join(peerReader.Close(), peerWriter.Close()) })
				conn.transport = newIOConnection(IOConfig{
					Reader: reader, Writer: writer,
					Close: func() error { return errors.Join(reader.Close(), writer.Close()) },
				}, false)
				conn.run()
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- operation(ctx, conn) }()

				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("the operation returned %v while its write was still in progress", err)
				default:
				}
				if err := conn.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				if err := <-done; err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("operation = %v, want the closed write's failure", err)
				}
				if err := conn.Wait(); err != nil {
					t.Errorf("Wait = %v, want nil for a local close", err)
				}
			})
		})
	}
}

func TestDrainingPreservesNotificationsAndCancelsRequestWork(t *testing.T) {
	for name, duringDelivery := range map[string]bool{"before delivery": false, "during delivery": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notifications := make(chan error, 1)
				requests := make(chan error, 1)
				entered := make(chan struct{})
				conn := newTestClientConn(t, ClientConfig{
					NotifyFallback: func(ctx context.Context, _ *ClientConn, notification *ExtNotification) {
						if notification.Method == "_test/active" {
							close(entered)
							<-ctx.Done()
							return
						}
						notifications <- ctx.Err()
					},
					CallFallback: func(ctx context.Context, _ *ClientConn, _ *ExtRequest) (any, error) {
						requests <- ctx.Err()
						return json.RawMessage(`{}`), nil
					},
				})
				conn.transport = &invalidMessageStream{}
				if duringDelivery && !conn.admit(rawNotification("_test/active", `{}`)) {
					t.Fatal("could not enqueue the active notification")
				}
				if !conn.admit(rawNotification("_test/final", `{}`)) || !conn.admit(rawCall(1, "_test/work", `{}`)) {
					t.Fatal("could not enqueue the messages")
				}
				if duringDelivery {
					conn.life.start(conn.deliverLoop)
					<-entered
				}
				conn.endReading(nil)
				close(conn.life.readEnded)
				if !duringDelivery {
					conn.life.start(conn.deliverLoop)
				}
				if err := conn.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := <-notifications; err != nil {
					t.Errorf("a queued notification inherited shutdown cancellation: %v", err)
				}
				if err := <-requests; !errors.Is(err, context.Canceled) {
					t.Errorf("queued request work received %v, want cancellation", err)
				}
			})
		})
	}
}

// A transport may need handler work to stop before its Close can finish. The
// connection owns both, so cancellation must flow outward before transport
// release rather than making each side wait for the other.
func TestLifetimeCancelsWorkBeforeReleasingTheTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		life := newLifetime()
		escape := make(chan struct{})
		cancelledBeforeRelease := make(chan bool, 1)
		ended := make(chan struct{})
		go func() {
			life.endReading(nil, func() error {
				select {
				case <-life.ctx.Done():
					cancelledBeforeRelease <- true
				case <-escape:
					cancelledBeforeRelease <- false
				}
				return nil
			})
			close(ended)
		}()

		synctest.Wait()
		select {
		case cancelled := <-cancelledBeforeRelease:
			if !cancelled {
				t.Fatal("transport release began before connection work was cancelled")
			}
		default:
			close(escape)
			<-ended
			t.Fatal("transport release was waiting on work whose context was not cancelled")
		}
		<-ended
		life.finishDelivering(func() {})
		if err := life.wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

func TestClosingWaitsForTheReaderToFinishDeliveringItsLastResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newTestAgentConn(t)
		call, _ := conn.calls.begin(nil, nil)
		stream := &lastResponseStream{
			response: &jsonrpc.Response{ID: call.id, Result: json.RawMessage(`{}`)},
			entered:  make(chan struct{}), resume: make(chan struct{}),
		}
		conn.transport = stream
		conn.run()
		<-stream.entered
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-conn.over():
			t.Error("delivery ended while the reader still held a response")
		default:
		}
		close(stream.resume)
		if err := conn.Wait(); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-call.completed:
			if result.response != stream.response {
				t.Errorf("last response was lost: %+v", result)
			}
		default:
			t.Error("last response was not delivered")
		}
	})
}

// The barrier models a reader preempted after accepting its last response but
// before handing it to the link. Closing the stream cannot revoke that response.
type lastResponseStream struct {
	response *jsonrpc.Response
	entered  chan struct{}
	resume   chan struct{}
}

func (s *lastResponseStream) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	close(s.entered)
	<-s.resume
	return s.response, nil
}

func (*lastResponseStream) Write(context.Context, jsonrpc.Message) error { return nil }
func (*lastResponseStream) Close() error                                 { return nil }

func TestInvalidTransportMessagesEndTheConnectionWithoutPanicking(t *testing.T) {
	for name, message := range map[string]jsonrpc.Message{
		"nil": nil, "request": (*jsonrpc.Request)(nil), "response": (*jsonrpc.Response)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			conn := newTestAgentConn(t)
			conn.transport = &invalidMessageStream{message: message}
			conn.run()
			if err := conn.Wait(); err == nil {
				t.Fatal("invalid transport message ended the connection without an error")
			}
		})
	}
}

type invalidMessageStream struct{ message jsonrpc.Message }

func (s *invalidMessageStream) Read(context.Context) (jsonrpc.Message, error) {
	return s.message, nil
}

func (*invalidMessageStream) Write(context.Context, jsonrpc.Message) error { return nil }
func (*invalidMessageStream) Close() error                                 { return nil }
