package acp_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

func TestIOTransportClosesASharedReadWriteResourceOnce(t *testing.T) {
	resource, err := os.CreateTemp(t.TempDir(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := acp.NewIOTransport(&acp.IOConfig{
		Reader: resource, Writer: resource, Close: resource.Close,
	}).Connect(t.Context())
	if err != nil {
		t.Fatal(errors.Join(err, resource.Close()))
	}
	t.Cleanup(func() { _ = stream.Close() })
	for range 2 {
		if err := stream.Close(); err != nil {
			t.Errorf("closing the shared resource: %v", err)
		}
	}
	if _, err := resource.WriteString("closed"); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the shared resource is still writable: %v", err)
	}
}

func TestIOTransportSharesItsReleaseFailureAcrossConcurrentClosers(t *testing.T) {
	failure := errors.New("resource could not be released")
	var releases atomic.Int32
	stream, err := acp.NewIOTransport(&acp.IOConfig{
		Reader: strings.NewReader(""), Writer: io.Discard,
		Close: func() error {
			releases.Add(1)
			return failure
		},
	}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 4)
	for range cap(closed) {
		go func() { closed <- stream.Close() }()
	}
	for range cap(closed) {
		if err := <-closed; !errors.Is(err, failure) {
			t.Errorf("Close = %v, want the release failure", err)
		}
	}
	if calls := releases.Load(); calls != 1 {
		t.Errorf("release callback ran %d times, want once", calls)
	}
}

// A write waiting for the stream honours its context and returns the exact
// no-commit signal. A write that has begun is not interrupted by its context,
// because cancellation cannot prove which prefix reached the peer; Close ends it.
func TestByteStreamWritesHonourCancellationOnlyUntilTheyBegin(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "writing"
		if queued {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			writer := &blockedStreamWriter{entered: make(chan struct{}, 2), closed: make(chan struct{})}
			stream, err := acp.NewIOTransport(&acp.IOConfig{
				Reader: strings.NewReader(""), Writer: writer, Close: writer.Close,
			}).Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"_test/write"}`))
			if err != nil {
				t.Fatal(err)
			}
			first := make(chan error, 1)
			if queued {
				go func() { first <- stream.Write(t.Context(), message) }()
				<-writer.entered
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			returned := make(chan error, 1)
			go func() { returned <- stream.Write(ctx, message) }()
			if !queued {
				<-writer.entered
			}
			cancel()
			if queued {
				writeErr := <-returned
				if writeErr != context.Canceled { //nolint:errorlint // exact identity promises no bytes were committed.
					t.Errorf("a queued write returned %v instead of the no-commit signal", writeErr)
				}
			} else {
				select {
				case writeErr := <-returned:
					t.Fatalf("a write in progress returned %v when its context was cancelled", writeErr)
				case <-time.After(50 * time.Millisecond):
				}
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if queued {
				<-first
			} else if writeErr := <-returned; writeErr == nil {
				t.Error("a write ended by Close reported success")
			}
			if writes := writer.writes.Load(); writes != 1 {
				t.Errorf("underlying writes = %d, want only the first write", writes)
			}
		})
	}
}

type blockedStreamWriter struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	writes  atomic.Int32
}

func (w *blockedStreamWriter) Write([]byte) (int, error) {
	w.writes.Add(1)
	w.entered <- struct{}{}
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockedStreamWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func TestAClosedByteStreamRefusesBufferedReadsAndFurtherWrites(t *testing.T) {
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"_test/write"}`))
	if err != nil {
		t.Fatal(err)
	}
	var writes strings.Builder
	stream, err := acp.NewIOTransport(&acp.IOConfig{
		Reader: strings.NewReader(`{"jsonrpc":"2.0","method":"_test/read"}`),
		Writer: &writes,
		Close:  func() error { return nil },
	}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(t.Context()); !errors.Is(err, acp.ErrConnectionClosed) {
		t.Errorf("Read after Close = %v", err)
	}
	if err := stream.Write(t.Context(), message); !errors.Is(err, acp.ErrConnectionClosed) {
		t.Errorf("Write after Close = %v", err)
	}
	if writes.Len() != 0 {
		t.Fatal("closed stream wrote bytes")
	}
}

func TestAByteStreamReportsAShortWrite(t *testing.T) {
	stream, err := acp.NewIOTransport(&acp.IOConfig{
		Reader: strings.NewReader(""), Writer: shortStreamWriter{}, Close: func() error { return nil },
	}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close() //nolint:errcheck // the test streams have no close errors.
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"_test/write"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Write(t.Context(), message); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("Write = %v, want io.ErrShortWrite", err)
	}
}

type shortStreamWriter struct{}

func (shortStreamWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestAnAlreadyCancelledMemoryTransportDoesNotConnect(t *testing.T) {
	left, _ := acp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := left.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect = %v, want context.Canceled", err)
	}
}
