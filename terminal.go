package acp

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// A TerminalHandle is one terminal the client is running for a session. It binds
// the session and terminal identifiers all five terminal methods need. Copies of
// a handle share its release state.
//
// It is named TerminalHandle because the schema already defines Terminal, the
// payload of a tool call's terminal content.
type TerminalHandle struct {
	id      TerminalID
	session *AgentSession
	state   *terminalState
}

// terminalState serializes admission and the transport write: an operation
// admitted before Release cannot be sent after the client has reused the
// identifier. Waiting for a response does not hold the gate, so Kill and Release
// can unblock WaitForExit.
type terminalState struct {
	sending  chan struct{}
	released bool
}

// ErrTerminalReleased is returned by an operation on a released terminal handle.
// Once a Release reaches the transport it is one-way, even if the response is an
// error: the client may have acted, and a retry could release a terminal that
// has since been given the same identifier.
var ErrTerminalReleased = errors.New("acp: terminal handle is released")

func (t *TerminalHandle) ID() TerminalID { return t.id }

func (t *TerminalHandle) Session() *AgentSession { return t.session }

// Output reports what the command has written so far, whether it was truncated,
// and the exit status once there is one. It does not wait.
func (t *TerminalHandle) Output(ctx context.Context, params *TerminalOutputParams) (*TerminalOutputResponse, error) {
	return callTerminal[TerminalOutputResponse](ctx, t, methodTerminalOutput, params.bind(t.session.id, t.id))
}

// WaitForExit blocks until the command exits. The terminal stays readable.
func (t *TerminalHandle) WaitForExit(
	ctx context.Context,
	params *WaitForTerminalExitParams,
) (*WaitForTerminalExitResponse, error) {
	return callTerminal[WaitForTerminalExitResponse](ctx, t, methodTerminalWaitForExit,
		params.bind(t.session.id, t.id))
}

// Kill stops the command without releasing the terminal.
func (t *TerminalHandle) Kill(ctx context.Context, params *KillTerminalParams) (*KillTerminalResponse, error) {
	return callTerminal[KillTerminalResponse](ctx, t, methodTerminalKill, params.bind(t.session.id, t.id))
}

// Release frees the terminal. Once the request is sent every operation on the
// handle, including a second Release, returns [ErrTerminalReleased]; a refusal
// before the transport leaves the handle usable so the caller can retry.
func (t *TerminalHandle) Release(
	ctx context.Context,
	params *ReleaseTerminalParams,
) (*ReleaseTerminalResponse, error) {
	call, err := t.admit(ctx, func() (outboundCall, error) {
		call, sendErr := t.session.conn.sendStandard(ctx, methodTerminalRelease, params.bind(t.session.id, t.id),
			nil, nil)
		if sendErr == nil || t.session.conn.life.ended() {
			t.state.released = true
		}
		return call, sendErr
	})
	if err != nil {
		return nil, err
	}
	return awaitFor[ReleaseTerminalResponse](ctx, t.session.conn.link, call)
}

func callTerminal[Response any](ctx context.Context, t *TerminalHandle, method string, request any) (*Response, error) {
	call, err := t.admit(ctx, func() (outboundCall, error) {
		return t.session.conn.sendStandard(ctx, method, request, nil, nil)
	})
	if err != nil {
		return nil, err
	}
	return awaitFor[Response](ctx, t.session.conn.link, call)
}

// admit runs send under the handle's gate, refusing a released handle.
func (t *TerminalHandle) admit(ctx context.Context, send func() (outboundCall, error)) (outboundCall, error) {
	select {
	case t.state.sending <- struct{}{}:
		defer func() { <-t.state.sending }()
	case <-ctx.Done():
		return outboundCall{}, ctx.Err()
	}
	if t.state.released {
		return outboundCall{}, ErrTerminalReleased
	}
	return send()
}

// A terminalCreation owns a created terminal until the caller takes it or, when
// the caller has left, a cleanup task releases it.
type terminalCreation struct {
	session *AgentSession

	mu        sync.Mutex
	abandoned bool
	handle    *TerminalHandle
	response  *CreateTerminalResponse
}

func (c *terminalCreation) accept(answer *jsonrpc.Response) error {
	response := new(CreateTerminalResponse)
	if err := decodeResponse(answer, response); err != nil {
		return err
	}
	handle := &TerminalHandle{
		id: response.TerminalID, session: c.session,
		state: &terminalState{sending: make(chan struct{}, 1)},
	}
	c.mu.Lock()
	abandoned := c.abandoned
	if !abandoned {
		c.handle, c.response = handle, response
	}
	c.mu.Unlock()
	if abandoned {
		c.release(handle)
	}
	return nil
}

func (c *terminalCreation) take() (*TerminalHandle, *CreateTerminalResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	handle, response := c.handle, c.response
	c.handle, c.response = nil, nil
	return handle, response
}

func (c *terminalCreation) abandon() {
	c.mu.Lock()
	c.abandoned = true
	handle := c.handle
	c.handle, c.response = nil, nil
	c.mu.Unlock()
	if handle != nil {
		c.release(handle)
	}
}

func (c *terminalCreation) release(handle *TerminalHandle) {
	conn := c.session.conn
	conn.life.start(func() {
		ctx, cancel := conn.life.detached()
		defer cancel()
		if err := releaseAbandonedTerminal(ctx, handle); err != nil && !errors.Is(err, ErrConnectionClosed) {
			conn.logger.Warn("releasing a terminal from an abandoned create request failed",
				slog.String("terminalId", string(handle.id)), slog.Any("error", err))
		}
	})
}

// releaseAbandonedTerminal retries only a release refused for pending-call
// capacity, which proves nothing was written and so cannot reach a terminal
// whose identifier the client has already reused.
func releaseAbandonedTerminal(ctx context.Context, handle *TerminalHandle) error {
	for {
		_, err := handle.Release(ctx, nil)
		if !errors.Is(err, errTooManyPendingCalls) {
			return err
		}
		if waitErr := handle.session.conn.calls.awaitCapacity(ctx); waitErr != nil {
			return waitErr
		}
	}
}
