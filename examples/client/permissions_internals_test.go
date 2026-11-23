package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp"
)

func permissionRequest(title string) *acp.RequestPermissionRequest {
	return &acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallID: acp.ToolCallID(title), Title: acp.OptValue(title)},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow"},
			{OptionID: "deny", Kind: acp.PermissionOptionKindRejectOnce, Name: "Deny"},
		},
	}
}

func TestPermissionRequestsShareOneInputOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		input, writer := io.Pipe()
		defer func() { _ = input.Close() }()
		defer func() { _ = writer.Close() }()
		var output bytes.Buffer
		prompt := newPermissionPrompt(ctx, input, &output)
		first := make(chan *acp.RequestPermissionResponse, 1)
		second := make(chan *acp.RequestPermissionResponse, 1)
		go func() {
			answer, err := prompt.requestPermission(ctx, nil, permissionRequest("first"))
			if err != nil {
				t.Error(err)
			}
			first <- answer
		}()
		synctest.Wait()
		go func() {
			answer, err := prompt.requestPermission(ctx, nil, permissionRequest("second"))
			if err != nil {
				t.Error(err)
			}
			second <- answer
		}()
		synctest.Wait()
		if strings.Contains(output.String(), "second") {
			t.Fatal("displayed another question before the first answer")
		}
		if _, err := io.WriteString(writer, "1\n"); err != nil {
			t.Fatal(err)
		}
		if answer := <-first; answer.Outcome.(*acp.SelectedPermissionOutcome).OptionID != "allow" {
			t.Fatalf("first answer = %#v", answer)
		}
		synctest.Wait()
		if _, err := io.WriteString(writer, "2\n"); err != nil {
			t.Fatal(err)
		}
		if answer := <-second; answer.Outcome.(*acp.SelectedPermissionOutcome).OptionID != "deny" {
			t.Fatalf("second answer = %#v", answer)
		}
	})
}

func TestCancelledPermissionDiscardsItsLateLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		input, writer := io.Pipe()
		defer func() { _ = input.Close() }()
		defer func() { _ = writer.Close() }()
		var output bytes.Buffer
		prompt := newPermissionPrompt(ctx, input, &output)
		firstCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		first := make(chan *acp.RequestPermissionResponse, 1)
		go func() {
			answer, err := prompt.requestPermission(firstCtx, nil, permissionRequest("cancelled question"))
			if err != nil {
				t.Error(err)
			}
			first <- answer
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case answer := <-first:
			if _, cancelled := answer.Outcome.(*acp.RequestPermissionOutcomeCancelled); !cancelled {
				t.Fatalf("cancelled request = %#v", answer)
			}
		default:
			t.Fatal("cancelled handler is still waiting for stdin")
		}
		second := make(chan *acp.RequestPermissionResponse, 1)
		go func() {
			answer, err := prompt.requestPermission(ctx, nil, permissionRequest("next question"))
			if err != nil {
				t.Error(err)
			}
			second <- answer
		}()
		synctest.Wait()
		if strings.Contains(output.String(), "next question") {
			t.Fatal("next question became visible while an old read still owned its answer")
		}
		if _, err := io.WriteString(writer, "1\n"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case answer := <-second:
			t.Fatalf("late approval answered a different question: %#v", answer)
		default:
		}
		if !strings.Contains(output.String(), "next question") {
			t.Fatal("draining the old answer did not advance the prompt")
		}
		if _, err := io.WriteString(writer, "2\n"); err != nil {
			t.Fatal(err)
		}
		if answer := <-second; answer.Outcome.(*acp.SelectedPermissionOutcome).OptionID != "deny" {
			t.Fatalf("next answer = %#v", answer)
		}
	})
}

func TestPermissionAcceptsACompleteChoiceAtEOF(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  acp.PermissionOptionID
	}{
		{name: "final choice without newline", input: "1", want: "allow"},
		{name: "empty EOF", input: "", want: "deny"},
	} {
		t.Run(test.name, func(t *testing.T) {
			answer, err := choosePermission(bufio.NewReader(strings.NewReader(test.input)), io.Discard, permissionRequest("append"))
			if err != nil {
				t.Fatal(err)
			}
			if got := answer.Outcome.(*acp.SelectedPermissionOutcome).OptionID; got != test.want {
				t.Fatalf("selected %q, want %q", got, test.want)
			}
		})
	}
}
