package acp

import (
	"context"
	"errors"
	"sync/atomic"
)

// The operations an agent performs on a client's workspace: the filesystem, and
// terminals. Each is gated on a capability the client advertised, and the gate is
// checked on both sides — outbound because the peer never offered it, inbound
// because the caller was told it was not there.

// ReadTextFile reads a text file from the client's workspace.
//
// Gated on clientCapabilities.fs.readTextFile.
func (s *AgentSession) ReadTextFile(
	ctx context.Context,
	params *ReadTextFileParams,
) (*ReadTextFileResponse, error) {
	if params == nil {
		return nil, errors.New("acp: ReadTextFile needs params: the path is in them")
	}
	request := &ReadTextFileRequest{
		SessionID: s.id,
		Path:      params.Path,
		Line:      params.Line,
		Limit:     params.Limit,
		Meta:      params.Meta,
	}
	return callGated[ReadTextFileResponse](ctx, s.conn, methodFsReadTextFile, request)
}

// WriteTextFile writes a text file in the client's workspace.
//
// Gated on clientCapabilities.fs.writeTextFile, which is a second boolean: reading
// and writing are two capabilities and not one.
func (s *AgentSession) WriteTextFile(
	ctx context.Context,
	params *WriteTextFileParams,
) (*WriteTextFileResponse, error) {
	if params == nil {
		return nil, errors.New("acp: WriteTextFile needs params: the path and the content are in them")
	}
	request := &WriteTextFileRequest{
		SessionID: s.id,
		Path:      params.Path,
		Content:   params.Content,
		Meta:      params.Meta,
	}
	return callGated[WriteTextFileResponse](ctx, s.conn, methodFsWriteTextFile, request)
}

// CreateTerminal runs a command in the client's workspace and returns a handle to
// it.
//
// Gated on clientCapabilities.terminal, one boolean covering all five terminal
// methods — so a client that advertises it implements all five, and a handle from
// here can use any of them.
//
// The response is returned as well as the handle, because it carries _meta besides
// the identifier and returning only a handle would make that unreachable.
func (s *AgentSession) CreateTerminal(
	ctx context.Context,
	params *CreateTerminalParams,
) (*TerminalHandle, *CreateTerminalResponse, error) {
	if params == nil {
		return nil, nil, errors.New("acp: CreateTerminal needs params: the command is in them")
	}
	request := &CreateTerminalRequest{
		SessionID:       s.id,
		Command:         params.Command,
		Args:            params.Args,
		Env:             params.Env,
		Cwd:             params.Cwd,
		OutputByteLimit: params.OutputByteLimit,
		Meta:            params.Meta,
	}
	response, err := callGated[CreateTerminalResponse](ctx, s.conn, methodTerminalCreate, request)
	if err != nil {
		return nil, nil, err
	}
	return &TerminalHandle{id: response.TerminalID, session: s}, response, nil
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

	// released is set by Release, once and for good.
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
// Release is one-way, and the first caller to reach it wins: a concurrent or
// later Release gets this too. A Release that fails on the wire still leaves the
// handle released, because the request went out and the client may have acted on
// it — retrying could release a terminal the client has since given the same
// identifier to.
var ErrTerminalReleased = errors.New("acp: this terminal has been released")

// ID is the identifier the client gave this terminal.
func (t *TerminalHandle) ID() TerminalID { return t.id }

// Session is the session this terminal belongs to.
func (t *TerminalHandle) Session() *AgentSession { return t.session }

// Output reports what the command has written so far, and whether it has exited.
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

// WaitForExit blocks until the command exits.
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
//
// It returns a result rather than only an error, and so does [TerminalHandle.Release],
// because the schema's response for each is an object carrying optional _meta.
// Returning only an error would throw the peer's data away — an empty-looking
// response is not a discardable one.
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

// Release frees the terminal and everything it holds. The handle is not usable
// afterwards: every operation on it, including a second Release, returns
// [ErrTerminalReleased].
func (t *TerminalHandle) Release(
	ctx context.Context,
	params *ReleaseTerminalParams,
) (*ReleaseTerminalResponse, error) {
	if !t.released.CompareAndSwap(false, true) {
		return nil, ErrTerminalReleased
	}
	request := &ReleaseTerminalRequest{SessionID: t.session.id, TerminalID: t.id}
	if params != nil {
		request.Meta = params.Meta
	}
	return callGated[ReleaseTerminalResponse](ctx, t.session.conn, methodTerminalRelease, request)
}

// callGated makes a call the peer must have advertised.
//
// The check is local and happens before the write. A call the peer never offered
// is refused here rather than sent and refused there: the peer's answer would be
// the same, and asking wastes a round trip while making a developer read a wire
// trace to find out what they forgot.
func callGated[Response any](ctx context.Context, c *AgentConn, method string, request any) (*Response, error) {
	if err := c.awaitHandshake(ctx, method); err != nil {
		return nil, err
	}
	if err := c.Peer().permits(method); err != nil {
		return nil, err
	}
	response := new(Response)
	if err := c.call(ctx, method, request, response); err != nil {
		return nil, err
	}
	return response, nil
}
