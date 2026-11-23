package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Tangerg/acp"
)

// One process-owned reader associates every line with the question displayed
// before that read. Cancellation releases a handler immediately; its late line
// is discarded before another question can be displayed. Stdin itself may not
// support interrupting a read, so no request owns or starts a reader goroutine.
type permissionPrompt struct {
	questions chan permissionQuestion
	stopped   <-chan struct{}
}

type permissionQuestion struct {
	cancelled <-chan struct{}
	request   *acp.RequestPermissionRequest
	answer    chan permissionAnswer
}

type permissionAnswer struct {
	response *acp.RequestPermissionResponse
	err      error
}

func newPermissionPrompt(ctx context.Context, input io.Reader, output io.Writer) *permissionPrompt {
	prompt := &permissionPrompt{questions: make(chan permissionQuestion), stopped: ctx.Done()}
	go prompt.serve(ctx, bufio.NewReader(input), output)
	return prompt
}

func (p *permissionPrompt) serve(ctx context.Context, input *bufio.Reader, output io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case question := <-p.questions:
			select {
			case <-question.cancelled:
				continue
			case <-ctx.Done():
				return
			default:
			}
			response, err := choosePermission(input, output, question.request)
			select {
			case <-question.cancelled:
				// The consumed line belongs to the cancelled question, never the
				// next caller queued at the terminal.
				continue
			case <-ctx.Done():
				return
			default:
				question.answer <- permissionAnswer{response: response, err: err}
			}
		}
	}
}

func (p *permissionPrompt) requestPermission(
	ctx context.Context,
	_ *acp.ClientConn,
	request *acp.RequestPermissionRequest,
) (*acp.RequestPermissionResponse, error) {
	question := permissionQuestion{
		cancelled: ctx.Done(), request: request, answer: make(chan permissionAnswer, 1),
	}
	select {
	case <-ctx.Done():
		return cancelledPermission(), nil
	case <-p.stopped:
		return cancelledPermission(), nil
	case p.questions <- question:
	}
	select {
	case <-ctx.Done():
		return cancelledPermission(), nil
	case <-p.stopped:
		return cancelledPermission(), nil
	case answer := <-question.answer:
		return answer.response, answer.err
	}
}

func cancelledPermission() *acp.RequestPermissionResponse {
	return &acp.RequestPermissionResponse{Outcome: &acp.RequestPermissionOutcomeCancelled{}}
}

func choosePermission(
	input *bufio.Reader,
	output io.Writer,
	request *acp.RequestPermissionRequest,
) (*acp.RequestPermissionResponse, error) {
	title, _ := request.ToolCall.Title.Get()
	lines := []string{fmt.Sprintf("\n\nthe agent wants to: %s\n", title)}
	for index, option := range request.Options {
		lines = append(lines, fmt.Sprintf("  %d) %s\n", index+1, option.Name))
	}
	lines = append(lines, "choose: ")
	if _, writeErr := io.WriteString(output, strings.Join(lines, "")); writeErr != nil {
		return nil, writeErr
	}
	line, err := input.ReadString('\n')
	if _, writeErr := fmt.Fprintln(output); writeErr != nil {
		return nil, writeErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	choice, numberErr := strconv.Atoi(strings.TrimSpace(line))
	if numberErr == nil && choice >= 1 && choice <= len(request.Options) {
		return &acp.RequestPermissionResponse{
			Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[choice-1].OptionID},
		}, nil
	}
	// An absent answer cannot authorize an operation. A refusal must be one the
	// agent offered; cancellation is reserved for the request actually ending.
	for _, option := range request.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce || option.Kind == acp.PermissionOptionKindRejectAlways {
			if _, writeErr := fmt.Fprintln(output, "declining"); writeErr != nil {
				return nil, writeErr
			}
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: option.OptionID},
			}, nil
		}
	}
	return nil, &acp.Error{Code: acp.ErrorCodeInternalError, Message: "nobody answered and no option means no"}
}
