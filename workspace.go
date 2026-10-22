package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The operations an agent performs on a client's workspace: the filesystem, and
// terminals. Each is gated on a capability the client advertised, and the gate is
// checked on both sides — outbound because the peer never offered it, inbound
// because the caller was told it was not there.

// The specification says every path the protocol carries must be absolute, so
// this package never sends one that is not. It is checked where the obligation
// is, on the way out: a receiver refusing a relative path is a policy an
// application may reasonably want to set for itself, and SECURITY.md already
// names a path from a peer as a boundary an application owns.
//
// filepath.IsAbs answers for the operating system this process is running on, and
// these paths describe the peer's filesystem. The ordinary deployment puts both
// peers on one machine, but a transport of a caller's own need not, so the check
// accepts what either convention calls absolute. That still refuses exactly what
// the rule exists to refuse: a path whose meaning depends on a working directory
// the two peers do not share.
func absolutePath(field, path string) error {
	if isAbsolutePath(path) {
		return nil
	}
	return newError(ErrorCodeInvalidParams,
		"%s must be an absolute path, and %q is not", field, path)
}

func isAbsolutePath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/"), strings.HasPrefix(path, `\\`):
		// A POSIX path, or a UNC share written either way.
		return true
	case len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\'):
		// A Windows drive letter.
		letter := path[0] | ' '
		return letter >= 'a' && letter <= 'z'
	default:
		return false
	}
}

func absoluteDirectories(directories []string) error {
	for i, directory := range directories {
		if err := absolutePath(fmt.Sprintf("additionalDirectories[%d]", i), directory); err != nil {
			return err
		}
	}
	return nil
}

// A TerminalHandle is one terminal the client is running for this session.
//
// It binds both identifiers the terminal methods need — the session's and the
// terminal's — so that neither is threaded through five calls by hand and neither
// can disagree with itself. Copies of a handle share its release state.
//
// The type is named TerminalHandle and not Terminal because the schema already
// defines Terminal: the payload of a tool call's terminal content. The schema's
// names are not this package's to reassign.
type TerminalHandle struct {
	id      TerminalID
	session *AgentSession
	state   *terminalState
}

type terminalState struct {
	// Admission and the transport write share one gate: an operation checked
	// before Release cannot send after the client has reused the identifier.
	// Waiting for a response does not hold it, so Kill and Release can unblock
	// WaitForExit. Only a proven no-commit failure leaves Release retryable.
	sending  chan struct{}
	released bool
}

// ErrTerminalReleased is returned by an operation on a terminal handle that has
// been released.
//
// Once a Release reaches the transport it is one-way, and a concurrent or later
// Release gets this too. A response error still leaves the handle released,
// because the client may have acted before producing it — retrying could release
// a terminal the client has since given the same identifier to.
var ErrTerminalReleased = errors.New("acp: terminal handle is released")

func (t *TerminalHandle) ID() TerminalID { return t.id }

func (t *TerminalHandle) Session() *AgentSession { return t.session }

// Output reports what the command has written so far, whether that output was
// truncated to the byte limit the terminal was created with, and the exit status
// once there is one. It does not wait; see [TerminalHandle.WaitForExit].
func (t *TerminalHandle) Output(
	ctx context.Context,
	params *TerminalOutputParams,
) (*TerminalOutputResponse, error) {
	request := &TerminalOutputRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callTerminal[TerminalOutputResponse](ctx, t, methodTerminalOutput, request)
}

// WaitForExit blocks until the command exits. The terminal is still readable
// afterwards, because exiting is not releasing.
func (t *TerminalHandle) WaitForExit(
	ctx context.Context,
	params *WaitForTerminalExitParams,
) (*WaitForTerminalExitResponse, error) {
	request := &WaitForTerminalExitRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callTerminal[WaitForTerminalExitResponse](ctx, t, methodTerminalWaitForExit, request)
}

// Kill stops the command without releasing the terminal, so its output can still
// be read.
func (t *TerminalHandle) Kill(
	ctx context.Context,
	params *KillTerminalParams,
) (*KillTerminalResponse, error) {
	request := &KillTerminalRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callTerminal[KillTerminalResponse](ctx, t, methodTerminalKill, request)
}

// Release frees the terminal and everything it holds. Once the request is sent,
// the handle is not usable afterwards: every operation on it, including a second
// Release, returns [ErrTerminalReleased]. A local refusal before transport
// delivery restores the handle so the caller can correct the context and retry.
func (t *TerminalHandle) Release(
	ctx context.Context,
	params *ReleaseTerminalParams,
) (*ReleaseTerminalResponse, error) {
	request := &ReleaseTerminalRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callTerminal[ReleaseTerminalResponse](ctx, t, methodTerminalRelease, request)
}

func callTerminal[Response any](
	ctx context.Context,
	t *TerminalHandle,
	method string,
	request any,
) (*Response, error) {
	call, err := t.send(ctx, method, request)
	if err != nil {
		return nil, err
	}
	response := new(Response)
	if awaitErr := t.session.conn.awaitInto(ctx, call, response); awaitErr != nil {
		return nil, awaitErr
	}
	return response, nil
}

func (t *TerminalHandle) send(ctx context.Context, method string, request any) (outboundCall, error) {
	select {
	case t.state.sending <- struct{}{}:
		defer func() { <-t.state.sending }()
	case <-ctx.Done():
		return outboundCall{}, ctx.Err()
	}
	if t.state.released {
		return outboundCall{}, ErrTerminalReleased
	}
	call, err := beginGatedCall(ctx, t.session.conn, method, request)
	if method == methodTerminalRelease && (err == nil || t.session.conn.ended()) {
		t.state.released = true
	}
	return call, err
}

// The gate is checked locally, before the write. A call the peer never offered
// would be refused there too, and asking wastes a round trip while making a
// developer read a wire trace to find out what they forgot.
func callGated[Response any](ctx context.Context, c *AgentConn, method string, request any) (*Response, error) {
	call, err := beginGatedCall(ctx, c, method, request)
	if err != nil {
		return nil, err
	}
	response := new(Response)
	if awaitErr := c.awaitInto(ctx, call, response); awaitErr != nil {
		return nil, awaitErr
	}
	return response, nil
}

// beginGatedCall stops before the transport on every local refusal. Release uses
// that boundary to know whether its one-way state may still be rolled back; the
// ordinary workspace operations immediately await the returned call.
func beginGatedCall(
	ctx context.Context,
	c *AgentConn,
	method string,
	request any,
) (outboundCall, error) {
	if err := c.awaitHandshake(ctx, method); err != nil {
		return outboundCall{}, err
	}
	if err := c.Peer().permits(method); err != nil {
		return outboundCall{}, err
	}
	return c.send(ctx, method, request, nil, nil)
}
