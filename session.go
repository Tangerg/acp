package acp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Tangerg/acp/jsonrpc"
)

// ErrPromptInProgress refuses a second concurrent prompt on one session.
// session/cancel carries only a session identifier, which is coherent only if a
// session has at most one active turn. A session is still safe for concurrent
// use; it is overlapping prompts that are refused.
var ErrPromptInProgress = errors.New("acp: prompt already in progress for this session")

// A ClientSession is one conversation, as the client sees it. It binds the
// session identifier to its connection and holds no other state, so two handles
// for one session are interchangeable: compare them by [ClientSession.ID]. A
// [SessionID] outlives its connection; a handle does not, so reopening a session
// on another connection gives a handle bound to that one.
type ClientSession struct {
	id   SessionID
	conn *ClientConn
}

func (s *ClientSession) ID() SessionID { return s.id }

func (s *ClientSession) Conn() *ClientConn { return s.conn }

// Prompt runs one turn and blocks until it ends. While it is outstanding the
// agent streams session/update notifications and may call back into this client.
//
// Cancelling ctx stops this call waiting and cancels the JSON-RPC request; it
// does not end the turn, which outlives the call until the agent answers.
// [ClientSession.Cancel] ends a turn. A prompt started while a cancellation of
// the same session is still being written waits for it to finish.
func (s *ClientSession) Prompt(ctx context.Context, params *PromptParams) (*PromptResponse, error) {
	turn, err := s.conn.turns.begin(ctx, s.id)
	if err != nil {
		return nil, err
	}
	// The response observer ends the turn even after this caller has left, and
	// losing the connection without an answer ends it too.
	call, err := s.conn.sendStandard(ctx, methodSessionPrompt, params.bind(s.id),
		func(*jsonrpc.Response) error {
			turn.complete()
			return nil
		}, turn.complete)
	turn.finishSending()
	if err != nil {
		turn.complete()
		return nil, err
	}
	result := new(PromptResponse)
	if awaitErr := s.conn.awaitInto(ctx, call, result); awaitErr != nil {
		return nil, awaitErr
	}
	return result, nil
}

// Cancel ends the current turn. The agent must still answer the outstanding
// prompt, with the cancelled stop reason, and may send final tool-call updates
// before it does, so a client keeps accepting session/update after cancelling.
//
// The cancellation is committed once it starts: ctx gates only whether it
// starts. Pending permission requests for the session are answered cancelled
// before session/cancel is sent, and the notification waits for the prompt it
// names to be on the wire; a failure to write it ends the connection, because
// the two peers would otherwise disagree about the turn.
func (s *ClientSession) Cancel(ctx context.Context, params *CancelParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn := s.conn
	cancellation := conn.turns.beginCancellation(s.id)
	defer cancellation.finish()
	for _, call := range cancellation.pending {
		//nolint:contextcheck // a committed cancellation's answers are the connection's to write.
		if write := conn.answerCancelled(call); write != nil {
			write()
			continue
		}
		// The handler won the answer immediately before cancellation; its answer
		// still has to reach the wire before session/cancel.
		<-call.settled
	}
	select {
	case <-cancellation.promptSent():
	case <-conn.over():
		return conn.life.failure()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(conn.life.ctx), responseWriteTimeout)
	defer cancel()
	//nolint:contextcheck // a committed cancellation is the connection's to finish, past ctx.
	if err := conn.notifyStandard(writeCtx, methodSessionCancel, params.bind(s.id)); err != nil {
		conn.logger.Error("sending session/cancel failed; ending the connection", slog.Any("error", err))
		conn.endReading(err)
		return err
	}
	return nil
}

// SetMode switches the agent's mode for this session. No capability gates it:
// an agent offers modes by returning them from session/new.
func (s *ClientSession) SetMode(ctx context.Context, params *SetSessionModeParams) (*SetSessionModeResponse, error) {
	return callFor[SetSessionModeResponse](ctx, &s.conn.connection, methodSessionSetMode, params.bind(s.id))
}

// SetConfigOption sets one of the configuration options the agent offered.
func (s *ClientSession) SetConfigOption(
	ctx context.Context,
	params *SetSessionConfigOptionParams,
) (*SetSessionConfigOptionResponse, error) {
	return callFor[SetSessionConfigOptionResponse](ctx, &s.conn.connection, methodSessionSetConfigOption,
		params.bind(s.id))
}

// Close ends the session and frees what the agent holds for it. The agent
// cancels work still running for the session first, so an outstanding Prompt
// answers with the cancelled stop reason. An agent answering resource-not-found
// has no such session, which leaves the same end state, so that is a successful
// close.
//
// Gated on agentCapabilities.sessionCapabilities.close.
func (s *ClientSession) Close(ctx context.Context, params *CloseSessionParams) (*CloseSessionResponse, error) {
	response, err := callFor[CloseSessionResponse](ctx, &s.conn.connection, methodSessionClose, params.bind(s.id))
	if errors.Is(err, &Error{Code: ErrorCodeResourceNotFound}) {
		return new(CloseSessionResponse), nil
	}
	return response, err
}

// An AgentSession is the same conversation as the agent sees it: a different
// set of operations, so a different type. It binds the session identifier to its
// connection and holds no other state. A handler may keep it beyond its own
// call, to send session/update from spawned work; it must not outlive the
// connection.
type AgentSession struct {
	id   SessionID
	conn *AgentConn
}

func (s *AgentSession) ID() SessionID { return s.id }

func (s *AgentSession) Conn() *AgentConn { return s.conn }

// Update sends the client one piece of a turn's output.
//
// After the client cancels the turn its handler's context is done, and an
// update sent with it fails; final tool-call updates for a cancelled turn are
// sent with a context that outlives it, such as context.WithoutCancel.
func (s *AgentSession) Update(ctx context.Context, params *SessionUpdateParams) error {
	return s.conn.notifyStandard(ctx, methodSessionUpdate, params.bind(s.id))
}

// RequestPermission asks the user to approve a tool call. A client that cancels
// the turn answers it with the cancelled outcome, so it never hangs on a turn
// that has ended. An outcome selecting an option that was not offered is refused.
func (s *AgentSession) RequestPermission(
	ctx context.Context,
	params *RequestPermissionParams,
) (*RequestPermissionResponse, error) {
	request := deepCopy(params.bind(s.id))
	offered, err := ownPermissionOptions(request.Options)
	if err != nil {
		return nil, err
	}
	response, err := callFor[RequestPermissionResponse](ctx, &s.conn.connection, methodSessionRequestPermission,
		request)
	if err != nil {
		return nil, err
	}
	if invalid := offered.validate(response.Outcome); invalid != nil {
		return nil, invalid
	}
	return response, nil
}

// CreateElicitation asks the user for structured input within this session.
func (s *AgentSession) CreateElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
) (*CreateElicitationResponse, error) {
	return s.conn.createElicitation(ctx, params, sessionScope{session: s.id}, nil)
}

// ReadTextFile reads a text file from the client's workspace.
//
// Gated on clientCapabilities.fs.readTextFile.
func (s *AgentSession) ReadTextFile(ctx context.Context, params *ReadTextFileParams) (*ReadTextFileResponse, error) {
	return callFor[ReadTextFileResponse](ctx, &s.conn.connection, methodFsReadTextFile, params.bind(s.id))
}

// WriteTextFile writes a text file in the client's workspace.
//
// Gated on clientCapabilities.fs.writeTextFile.
func (s *AgentSession) WriteTextFile(
	ctx context.Context,
	params *WriteTextFileParams,
) (*WriteTextFileResponse, error) {
	return callFor[WriteTextFileResponse](ctx, &s.conn.connection, methodFsWriteTextFile, params.bind(s.id))
}

// CreateTerminal runs a command in the client's workspace and returns a handle to
// it, with the response for its _meta.
//
// Cancellation can race a terminal the client already created. A late success
// is released on a bounded cleanup budget, because the caller can no longer own
// it; a cleanup failure is logged.
//
// Gated on clientCapabilities.terminal.
func (s *AgentSession) CreateTerminal(
	ctx context.Context,
	params *CreateTerminalParams,
) (*TerminalHandle, *CreateTerminalResponse, error) {
	creation := &terminalCreation{session: s}
	call, err := s.conn.sendStandard(ctx, methodTerminalCreate, params.bind(s.id), creation.accept, nil)
	if err != nil {
		return nil, nil, err
	}
	if _, awaitErr := s.conn.await(ctx, call); awaitErr != nil {
		creation.abandon() //nolint:contextcheck // cleanup owns a new deadline after the caller leaves.
		return nil, nil, awaitErr
	}
	handle, response := creation.take()
	return handle, response, nil
}
