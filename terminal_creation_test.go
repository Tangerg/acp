package acp_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp"
)

func TestCancelledTerminalCreationReleasesLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		created := make(chan struct{})
		answer := make(chan struct{})
		finish := make(chan struct{})
		cancelCreate := make(chan context.CancelFunc, 1)
		callReturned := make(chan error, 1)
		var live atomic.Bool
		var released atomic.Int32
		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
			RequestPermission: denyingPermission,
			Terminal: &acp.TerminalHandlers{
				Create: func(context.Context, *acp.ClientConn, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
					// Resource creation commits before a reply can cross the transport.
					live.Store(true)
					close(created)
					<-answer
					return &acp.CreateTerminalResponse{TerminalID: "late-terminal"}, nil
				},
				Output: func(context.Context, *acp.ClientConn, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
					return &acp.TerminalOutputResponse{}, nil
				},
				WaitForExit: func(context.Context, *acp.ClientConn, *acp.WaitForTerminalExitRequest) (*acp.WaitForTerminalExitResponse, error) {
					return &acp.WaitForTerminalExitResponse{}, nil
				},
				Kill: func(context.Context, *acp.ClientConn, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
					return &acp.KillTerminalResponse{}, nil
				},
				Release: func(context.Context, *acp.ClientConn, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
					released.Add(1)
					live.Store(false)
					return &acp.ReleaseTerminalResponse{}, nil
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			createCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			cancelCreate <- cancel
			handle, response, err := session.CreateTerminal(createCtx, &acp.CreateTerminalParams{Command: "/bin/example"})
			if handle != nil || response != nil {
				t.Error("cancelled caller received a terminal owner")
			}
			callReturned <- err
			<-finish
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		})
		session := connectAndOpen(t, client, agent)
		promptDone := make(chan error, 1)
		go func() { _, err := session.Prompt(t.Context(), nil); promptDone <- err }()
		<-created
		(<-cancelCreate)()
		if err := <-callReturned; !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateTerminal returned %v", err)
		}
		close(answer)
		synctest.Wait()
		if live.Load() || released.Load() != 1 {
			t.Errorf("terminal live = %t, release calls = %d; want false and one owner releasing", live.Load(), released.Load())
		}
		close(finish)
		if err := <-promptDone; err != nil {
			t.Fatal(err)
		}
	})
}
