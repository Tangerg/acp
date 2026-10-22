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

func TestByteStreamWritesRespectCancellationWhileQueuedAndWhileWriting(t *testing.T) {
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
			checked := &checkedWriteContext{Context: ctx, checked: make(chan struct{})}
			returned := make(chan error, 1)
			go func() { returned <- stream.Write(checked, message) }()
			if !queued {
				<-writer.entered
			}
			<-checked.checked
			cancel()
			completed := false
			select {
			case writeErr := <-returned:
				completed = true
				if !errors.Is(writeErr, context.Canceled) {
					t.Errorf("Write returned %v, want cancellation", writeErr)
				}
				if queued && writeErr != context.Canceled { //nolint:errorlint // exact identity promises no bytes were committed.
					t.Errorf("a queued write returned %v instead of the no-commit signal", writeErr)
				}
				if !queued && writeErr == context.Canceled { //nolint:errorlint // a started byte write cannot promise no commitment.
					t.Error("an interrupted byte write claimed no bytes were committed")
				}
			case <-time.After(5 * time.Second):
				t.Error("Write remained blocked after its context was cancelled")
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if queued {
				<-first
			}
			if !completed {
				<-returned
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

// Capture the initial check so cancellation happens after a writer entered Write,
// even if it has not yet acquired the stream gate.
type checkedWriteContext struct {
	context.Context //nolint:containedctx // wraps one test call to observe its cancellation check.
	checked         chan struct{}
	once            sync.Once
}

func (c *checkedWriteContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
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
