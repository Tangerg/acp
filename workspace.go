package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Tangerg/acp/jsonrpc"
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
// can disagree with itself.
//
// The type is named TerminalHandle and not Terminal because the schema already
// defines Terminal: the payload of a tool call's terminal content. The schema's
// names are not this package's to reassign.
type TerminalHandle struct {
	id      TerminalID
	session *AgentSession

	// released is claimed before Release starts so concurrent operations cannot
	// begin behind it. A failure before the request reaches the transport rolls the
	// claim back; after a successful write it is permanent because the client may
	// already have acted.
	//
	// The contract is enforced rather than documented because a released
	// identifier is the client's to reuse. An operation on a handle after release
	// is not merely pointless: it may name a terminal that now belongs to
	// something else, and the client would serve it.
	released atomic.Bool
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
	if t.released.Load() {
		return nil, ErrTerminalReleased
	}
	request := &TerminalOutputRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callGated[TerminalOutputResponse](ctx, t.session.conn, methodTerminalOutput, request)
}

// WaitForExit blocks until the command exits. The terminal is still readable
// afterwards, because exiting is not releasing.
func (t *TerminalHandle) WaitForExit(
	ctx context.Context,
	params *WaitForTerminalExitParams,
) (*WaitForTerminalExitResponse, error) {
	if t.released.Load() {
		return nil, ErrTerminalReleased
	}
	request := &WaitForTerminalExitRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callGated[WaitForTerminalExitResponse](ctx, t.session.conn, methodTerminalWaitForExit, request)
}

// Kill stops the command without releasing the terminal, so its output can still
// be read.
func (t *TerminalHandle) Kill(
	ctx context.Context,
	params *KillTerminalParams,
) (*KillTerminalResponse, error) {
	if t.released.Load() {
		return nil, ErrTerminalReleased
	}
	request := &KillTerminalRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callGated[KillTerminalResponse](ctx, t.session.conn, methodTerminalKill, request)
}

// Release frees the terminal and everything it holds. Once the request is sent,
// the handle is not usable afterwards: every operation on it, including a second
// Release, returns [ErrTerminalReleased]. A local refusal before transport
// delivery restores the handle so the caller can correct the context and retry.
func (t *TerminalHandle) Release(
	ctx context.Context,
	params *ReleaseTerminalParams,
) (*ReleaseTerminalResponse, error) {
	if !t.released.CompareAndSwap(false, true) {
		return nil, ErrTerminalReleased
	}
	rollback := func() { t.released.CompareAndSwap(true, false) }
	request := &ReleaseTerminalRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	conn := t.session.conn
	response, call, err := beginGatedCall[ReleaseTerminalResponse](
		ctx, conn, methodTerminalRelease, request,
	)
	if err != nil {
		if !conn.ended() {
			rollback()
		}
		return nil, err
	}
	if err := conn.await(ctx, call); err != nil {
		return nil, err
	}
	return response, nil
}

// The gate is checked locally, before the write. A call the peer never offered
// would be refused there too, and asking wastes a round trip while making a
// developer read a wire trace to find out what they forgot.
func callGated[Response any](ctx context.Context, c *AgentConn, method string, request any) (*Response, error) {
	response, call, err := beginGatedCall[Response](ctx, c, method, request)
	if err != nil {
		return nil, err
	}
	if err := c.await(ctx, call); err != nil {
		return nil, err
	}
	return response, nil
}

// beginGatedCall stops before the transport on every local refusal. Release uses
// that boundary to know whether its one-way state may still be rolled back; the
// ordinary workspace operations immediately await the returned call.
func beginGatedCall[Response any](
	ctx context.Context,
	c *AgentConn,
	method string,
	request any,
) (*Response, outboundCall, error) {
	if err := c.awaitHandshake(ctx, method); err != nil {
		return nil, outboundCall{}, err
	}
	if err := c.Peer().permits(method); err != nil {
		return nil, outboundCall{}, err
	}
	response := new(Response)
	call, err := c.send(ctx, method, request, func(answer *jsonrpc.Response) error {
		return decodeResponse(answer, response)
	}, nil)
	if err != nil {
		return nil, outboundCall{}, err
	}
	return response, call, nil
}
