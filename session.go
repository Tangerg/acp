package acp

import (
	"context"
	"errors"

	"github.com/Tangerg/acp/jsonrpc"
)

// ErrPromptInProgress refuses a second concurrent prompt on one session.
//
// The protocol has no way to say which turn a session/cancel means: the
// notification carries only a session identifier — no turn and no request
// identifier. That is only coherent if a session has at most one active turn, so
// a second overlapping prompt is refused locally rather than left to scheduling,
// and pending permission requests can then be indexed by session without
// ambiguity.
//
// A session is safe for concurrent use; that is a separate question from whether
// two prompts may overlap, and the answer to the first is yes while the answer to
// the second is no.
var ErrPromptInProgress = errors.New("acp: prompt already in progress for this session")

// A ClientSession is one conversation, as the client sees it.
//
// It binds the session identifier and nothing else. Session identifiers appear
// across many protocol operations, so binding one prevents mismatched identifiers
// without expanding the handle's lifetime or responsibility.
//
// Handles are connection-bound. A [SessionID] outlives the connection it came
// from, but a handle does not: LoadSession on a new connection returns a new
// handle. A handle that silently re-pointed at another transport would be a
// lifetime nobody can reason about, so callers keep the identifier and ask for a
// fresh handle.
type ClientSession struct {
	id   SessionID
	conn *ClientConn
}

func (s *ClientSession) ID() SessionID { return s.id }

func (s *ClientSession) Conn() *ClientConn { return s.conn }

// Prompt runs one turn and blocks until it ends.
//
// While it is outstanding the agent streams session/update notifications and may
// call back into this client — for permission before a sensitive tool call, and
// for the filesystem and terminal operations that do the work. That is why both
// peers serve requests: an agent answering a prompt is simultaneously a caller.
//
// Cancelling ctx cancels the JSON-RPC request and returns ctx.Err(). It does not
// end the turn: [ClientSession.Cancel] does that, and the two are different
// operations for a reason the protocol insists on. See [ClientSession.Cancel].
//
// The turn outlives this call when that happens. The agent still owes an answer,
// and the session is not free for the next prompt until the connection accepts
// that answer. A caller who wants the session back promptly should call Cancel,
// which is the operation that ends a turn.
func (s *ClientSession) Prompt(ctx context.Context, params *PromptParams) (*PromptResponse, error) {
	conn := s.conn
	if params != nil {
		if err := conn.Peer().permitsPromptContent(params.Prompt); err != nil {
			return nil, err
		}
	}
	generation, claimed := conn.turns.begin(s.id)
	if !claimed {
		return nil, ErrPromptInProgress
	}

	request := &PromptRequest{SessionID: s.id}
	if params != nil {
		request.Prompt = params.Prompt
		request.Meta = params.Meta
	}

	result := new(PromptResponse)
	finish := func() { conn.turns.complete(s.id, generation) }
	call, err := conn.send(ctx, methodSessionPrompt, request, func(response *jsonrpc.Response) error {
		defer finish()
		return decodeResponse(response, result)
	}, finish)
	if err != nil {
		finish()
		return nil, err
	}

	select {
	case err := <-call.completed:
		if err != nil {
			return nil, err
		}
		return result, nil

	case <-conn.over():
		// An answer already in hand is an answer; see link.await.
		select {
		case err := <-call.completed:
			if err != nil {
				return nil, err
			}
			return result, nil
		default:
		}
		return nil, conn.failure()

	case <-ctx.Done():
		select {
		case err := <-call.completed:
			if err != nil {
				return nil, err
			}
			return result, nil
		default:
		}
		// This caller has stopped waiting. The turn has not stopped running: the
		// agent owes an answer and the protocol says what it is. Tell the agent,
		// on a budget of its own, and leave a waiter behind — the session is free
		// for the next prompt when that answer arrives and not before.
		//nolint:contextcheck // deliberate; the notification has a budget of its own.
		conn.cancelRemotely(call.id)
		return nil, ctx.Err()
	}
}

// Cancel ends the current turn.
//
// It is a notification, and sending it does not end the turn by itself: the
// protocol requires the agent to answer the outstanding session/prompt with the
// cancelled stop reason, and requires this client to keep accepting session/update
// until it does — the agent may still have final tool-call updates to report.
//
// This is not the same as cancelling a Prompt's context. That cancels one
// JSON-RPC request and stops this side waiting; this cancels a turn and leaves the
// request outstanding, on purpose, so that the agent can answer it.
func (s *ClientSession) Cancel(ctx context.Context, params *CancelParams) error {
	// The pending permission requests are claimed and answered first — before the
	// notification goes out and before this returns. See cancel.go.
	cancellation := s.conn.turns.beginCancellation(s.id)
	for _, id := range cancellation.pending {
		settled := s.conn.requests.settlement(id)
		//nolint:contextcheck // the answer is the connection's obligation, not this caller's.
		if write := s.conn.answerCancelled(id); write != nil {
			write()
			continue
		}
		// The permission handler may have won the answer claim immediately before
		// cancellation. Its response still has to reach the wire before
		// session/cancel; a claimed answer is not yet an observed one.
		if settled != nil {
			<-settled
		}
	}
	// And the turn is held until the notification is on the wire. It names only
	// the session, so a prompt that started before it went out would be the turn
	// the agent applies it to.
	defer s.conn.turns.finishCancellation(s.id, cancellation.generation)

	notification := &CancelNotification{SessionID: s.id}
	if params != nil {
		notification.Meta = params.Meta
	}
	return s.conn.notify(ctx, methodSessionCancel, notification)
}

// SetMode switches the agent's mode for this session.
//
// No capability gates it: an agent offers modes by returning them from
// [ClientConn.NewSession], so an agent that offered none has nothing to set and
// answers method-not-found.
func (s *ClientSession) SetMode(ctx context.Context, params *SetModeParams) (*SetSessionModeResponse, error) {
	request := &SetSessionModeRequest{SessionID: s.id}
	if params != nil {
		request.ModeID = params.ModeID
		request.Meta = params.Meta
	}
	response := new(SetSessionModeResponse)
	if err := s.conn.call(ctx, methodSessionSetMode, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// SetConfigOption sets one of the session configuration options the agent
// offered. Which options exist, and which values they take, come from the agent:
// session/new returns them and every answer here returns the full set again.
func (s *ClientSession) SetConfigOption(
	ctx context.Context,
	params *SetConfigOptionParams,
) (*SetSessionConfigOptionResponse, error) {
	if params == nil {
		return nil, paramsRequired("SetConfigOption", "ConfigID and Value")
	}
	if err := s.conn.Peer().permitsConfigOptionValue(params.Value); err != nil {
		return nil, err
	}
	request := &SetSessionConfigOptionRequest{
		SessionID: s.id,
		ConfigID:  params.ConfigID,
		Value:     params.Value,
		Meta:      params.Meta,
	}
	response := new(SetSessionConfigOptionResponse)
	if err := s.conn.call(ctx, methodSessionSetConfigOption, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// Close ends the session and frees what the agent holds for it.
//
// The agent must cancel any work still running for the session before it frees
// anything, so an outstanding [ClientSession.Prompt] answers with the cancelled
// stop reason rather than being abandoned. After success, the connection removes
// this handle from its identity cache. Callers should discard it; a later
// operation that reopens the same identifier receives a new handle.
//
// Gated on agentCapabilities.sessionCapabilities.close.
func (s *ClientSession) Close(ctx context.Context, params *CloseParams) (*CloseSessionResponse, error) {
	if err := s.conn.Peer().permits(methodSessionClose); err != nil {
		return nil, err
	}
	request := &CloseSessionRequest{SessionID: s.id}
	if params != nil {
		request.Meta = params.Meta
	}
	response := new(CloseSessionResponse)
	if err := s.conn.call(ctx, methodSessionClose, request, response); err != nil {
		return nil, err
	}
	s.conn.sessions.forget(s.id)
	return response, nil
}

// An AgentSession is the same conversation as the agent sees it.
//
// A different set of operations, so a different type. A client never calls
// RequestPermission and an agent never calls Prompt; one type carrying both method
// sets would make those calls compile and fail at runtime, which is a worse place
// to find out.
//
// An agent never constructs one: the connection hands it to the handlers whose
// requests carry a session identifier. The handle is valid for as long as the
// session is, not merely for the handler call — an agent that spawns work for a
// turn keeps the handle to send session/update from it, and that is the ordinary
// case rather than an escape. What it must not outlive is the connection: calling
// through it after Close returns [ErrConnectionClosed].
type AgentSession struct {
	id   SessionID
	conn *AgentConn
}

func (s *AgentSession) ID() SessionID { return s.id }

func (s *AgentSession) Conn() *AgentConn { return s.conn }

// Update sends the client one piece of a turn's output.
//
// A client keeps accepting these after it has sent a cancellation, because an
// agent may still have final tool-call updates to report before it answers the
// prompt.
func (s *AgentSession) Update(ctx context.Context, params *SessionUpdateParams) error {
	if params == nil {
		return paramsRequired("Update", "Update")
	}
	if err := s.conn.awaitHandshake(ctx, methodSessionUpdate); err != nil {
		return err
	}
	notification := &SessionNotification{
		SessionID: s.id,
		Update:    params.Update,
		Meta:      params.Meta,
	}
	return s.conn.notify(ctx, methodSessionUpdate, notification)
}

// RequestPermission asks the user to approve a tool call.
//
// A client that cancels the turn while this is outstanding answers it with the
// cancelled outcome rather than dropping it, so this returns an outcome either
// way and never hangs on a turn that has ended.
func (s *AgentSession) RequestPermission(
	ctx context.Context,
	params *RequestPermissionParams,
) (*RequestPermissionResponse, error) {
	if params == nil {
		return nil, paramsRequired("RequestPermission", "ToolCall and Options")
	}
	if err := s.conn.awaitHandshake(ctx, methodSessionRequestPermission); err != nil {
		return nil, err
	}
	request := &RequestPermissionRequest{
		SessionID: s.id,
		ToolCall:  params.ToolCall,
		Options:   params.Options,
		Meta:      params.Meta,
	}
	response := new(RequestPermissionResponse)
	if err := s.conn.call(ctx, methodSessionRequestPermission, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// CreateElicitation asks the user for structured input within this session.
func (s *AgentSession) CreateElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
) (*CreateElicitationResponse, error) {
	scope := func(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
		within := ElicitationSessionScope{SessionID: s.id, ToolCallID: params.ToolCallID}
		switch mode := mode.(type) {
		case *ElicitationFormMode:
			mode.Value = &ElicitationFormModeSession{ElicitationSessionScope: within}
		case *ElicitationURLMode:
			mode.Value = &ElicitationURLModeSession{ElicitationSessionScope: within}
		}
		return mode, nil
	}
	return createElicitation(ctx, s.conn, params, scope)
}

// ReadTextFile reads a text file from the client's workspace.
//
// Gated on clientCapabilities.fs.readTextFile.
func (s *AgentSession) ReadTextFile(
	ctx context.Context,
	params *ReadTextFileParams,
) (*ReadTextFileResponse, error) {
	if params == nil {
		return nil, paramsRequired("ReadTextFile", "Path")
	}
	if err := absolutePath("path", params.Path); err != nil {
		return nil, err
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
		return nil, paramsRequired("WriteTextFile", "Path and Content")
	}
	if err := absolutePath("path", params.Path); err != nil {
		return nil, err
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
		return nil, nil, paramsRequired("CreateTerminal", "Command")
	}
	if cwd, set := params.Cwd.Get(); set {
		if err := absolutePath("cwd", cwd); err != nil {
			return nil, nil, err
		}
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

// A sessionRequest is a request that names the session it belongs to, which is
// what lets one dispatch path hand a handle to every handler that needs one.
type sessionRequest interface {
	sessionID() SessionID
}

func (x *LoadSessionRequest) sessionID() SessionID            { return x.SessionID }
func (x *SetSessionModeRequest) sessionID() SessionID         { return x.SessionID }
func (x *SetSessionConfigOptionRequest) sessionID() SessionID { return x.SessionID }
func (x *ResumeSessionRequest) sessionID() SessionID          { return x.SessionID }

func dispatchSessionCall[Request sessionRequest, Response any](
	ctx context.Context,
	c *AgentConn,
	request *jsonrpc.Request,
	handle func(context.Context, *AgentSession, Request) (*Response, error),
) (any, error) {
	if handle == nil {
		return nil, methodNotImplemented(request.Method)
	}
	params, err := decodeParams[Request](request)
	if err != nil {
		return nil, err
	}
	response, err := handle(ctx, c.session((*params).sessionID()), *params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}
