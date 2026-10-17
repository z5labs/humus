// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errShuttingDown is the status a stream that Shutdown ended ends with. It
// is also the cause of the stream handler's context, so a handler that
// returns context.Cause of its context returns it too.
var errShuttingDown = status.Error(codes.Unavailable, "server is shutting down")

// streams ends every streaming RPC at Shutdown. Its interceptor gives each
// stream's handler a context of its own, derived from the RPC's, which
// stop cancels, with errShuttingDown as its cause.
type streams struct {
	// stopping is cancelled by stop. Each stream's context is cancelled
	// after it.
	stopping context.Context
	stop     context.CancelFunc
}

// newStreams returns a streams whose stop has not been called.
func newStreams() *streams {
	ctx, cancel := context.WithCancel(context.Background())
	return &streams{stopping: ctx, stop: cancel}
}

// interceptor returns the stream interceptor that gives the rest of the
// chain, the end user's interceptors and the handler, the stream's own
// context. Once stop has cancelled that context, a handler that returns
// nil, an error that is context.Canceled, or a status with codes.Canceled
// ends its stream with errShuttingDown instead. A handler that returns any
// other error keeps it.
//
// It cancels only the context. A RecvMsg that is blocked waiting for the
// client's next message stays blocked.
func (st *streams) interceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, cancel := context.WithCancelCause(ss.Context())
		defer cancel(nil)
		stopAfter := context.AfterFunc(st.stopping, func() { cancel(errShuttingDown) })
		defer stopAfter()
		// AfterFunc calls its function in a goroutine of its own, even when
		// stopping is done already, so a stream that starts after stop is
		// cancelled here, before its handler runs.
		if st.stopping.Err() != nil {
			cancel(errShuttingDown)
		}

		err := handler(srv, &serverStream{ServerStream: ss, ctx: ctx})
		if context.Cause(ctx) != errShuttingDown { //nolint:errorlint // the cause is errShuttingDown itself, or another
			return err
		}
		if err == nil || errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
			return errShuttingDown
		}
		return err
	}
}

// serverStream is a grpc.ServerStream whose Context is ctx.
type serverStream struct {
	grpc.ServerStream

	ctx context.Context
}

func (ss *serverStream) Context() context.Context {
	return ss.ctx
}
