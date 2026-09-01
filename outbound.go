package acp

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// A cancellation notification has its own budget because the caller's context
// is already done and a degraded peer must not delay that caller's return.
const cancelRequestTimeout = 5 * time.Second

func (l *link) call(ctx context.Context, method string, params, result any) error {
	call, err := l.send(ctx, method, params, func(response *jsonrpc.Response) error {
		return decodeResponse(response, result)
	}, nil)
	if err != nil {
		return err
	}
	return l.await(ctx, call)
}

// send is separate from await because a prompt remains a live turn after its
// original caller stops waiting.
func (l *link) send(
	ctx context.Context,
	method string,
	params any,
	accept func(*jsonrpc.Response) error,
	abandon func(),
) (outboundCall, error) {
	if err := ctx.Err(); err != nil {
		return outboundCall{}, err
	}
	call, open := l.calls.begin(accept, abandon)
	if !open {
		return call, l.life.failure()
	}
	request, err := jsonrpc2.NewCall(call.id, method, params)
	if err != nil {
		l.calls.retire(call.id)
		return call, err
	}
	if err := l.transport.Write(ctx, request); err != nil {
		l.calls.retire(call.id)
		return call, l.writeFailure(ctx, err)
	}
	return call, nil
}

// The initialize response is accepted on the delivery loop so a following
// message cannot observe an unpublished handshake.
func (l *link) callHandshake(
	ctx context.Context,
	method string,
	params any,
	publish func(*jsonrpc.Response) error,
) error {
	call, err := l.send(ctx, method, params, publish, nil)
	if err != nil {
		return err
	}
	return l.await(ctx, call)
}

func (l *link) await(ctx context.Context, call outboundCall) error {
	select {
	case err := <-call.completed:
		return err

	case <-ctx.Done():
		// Prefer an answer already delivered because response arrival and context
		// cancellation are routinely ready in the same scheduler turn.
		select {
		case err := <-call.completed:
			return err
		default:
		}
		l.calls.retire(call.id)
		//nolint:contextcheck // the caller is done; cancellation needs its own budget.
		l.cancelRemotely(call.id)
		return ctx.Err()

	case <-l.life.delivered:
		select {
		case err := <-call.completed:
			return err
		default:
		}
		return l.life.failure()
	}
}

func (l *link) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := jsonrpc2.NewNotification(method, params)
	if err != nil {
		return err
	}
	select {
	case <-l.life.delivered:
		return l.life.failure()
	default:
	}
	if err := l.transport.Write(ctx, request); err != nil {
		return l.writeFailure(ctx, err)
	}
	return nil
}

func (l *link) cancelRemotely(id jsonrpc.ID) {
	l.life.spawn(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), cancelRequestTimeout)
		defer cancel()

		params := &cancelRequestNotification{RequestID: requestIDOf(id)}
		if err := l.notify(ctx, methodCancelRequest, params); err != nil &&
			!errors.Is(err, ErrConnectionClosed) {
			l.logger.Warn("acp: telling the peer to cancel a request failed", slog.Any("error", err))
		}
	})
}

// Returning the caller's exact context error is the transport's proof that it
// committed no part of the message. A wrapped context error is deliberately not
// enough: once commitment is uncertain, another write could corrupt a byte stream.
func (l *link) writeFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && err == ctxErr { //nolint:errorlint // exact identity is the no-commit signal.
		return err
	}
	l.endReading(err)
	return l.failure()
}
