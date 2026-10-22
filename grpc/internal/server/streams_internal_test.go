// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// fakeStream is a grpc.ServerStream with only a context. Its RecvMsg
// blocks until release is closed, as a client that sends nothing does.
type fakeStream struct {
	grpc.ServerStream

	ctx     context.Context
	release chan struct{}
}

func (fs *fakeStream) Context() context.Context { return fs.ctx }

func (fs *fakeStream) RecvMsg(any) error {
	<-fs.release
	return errors.New("released")
}

var streamInfo = &grpc.StreamServerInfo{FullMethod: "/grpc.testing.TestService/FullDuplexCall", IsClientStream: true, IsServerStream: true}

func TestStreams_a_stream_that_starts_during_Shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		srv, err := New(&humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}, "example.com/caller", ":8081", nil)
		require.NoError(t, err)

		// The first Shutdown is running, waiting in GracefulStop for an
		// RPC, when the stream starts.
		srv.streams.stop()

		var handlerErr error
		err = srv.streams.interceptor()(nil, &fakeStream{ctx: t.Context()}, streamInfo, func(_ any, ss grpc.ServerStream) error {
			// Synchronously, before the handler does anything.
			handlerErr = ss.Context().Err()
			return ss.Context().Err()
		})
		require.ErrorIs(t, handlerErr, context.Canceled, "the handler's context is cancelled already")
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, "server is shutting down", status.Convert(err).Message())
	})

	t.Run("through Shutdown itself", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			srv, err := New(&humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}, "example.com/caller", ":8081", nil)
			require.NoError(t, err)
			require.NoError(t, srv.Shutdown(t.Context()))

			err = srv.streams.interceptor()(nil, &fakeStream{ctx: t.Context()}, streamInfo, func(_ any, ss grpc.ServerStream) error {
				require.Error(t, ss.Context().Err(), "the handler's context is cancelled already")
				return nil
			})
			require.Equal(t, codes.Unavailable, status.Code(err), err)
		})
	})
}

func TestStreams_interceptor(t *testing.T) {
	t.Run("leaves a stream alone before Shutdown", func(t *testing.T) {
		st := newStreams()
		err := st.interceptor()(nil, &fakeStream{ctx: t.Context()}, streamInfo, func(_ any, ss grpc.ServerStream) error {
			require.NoError(t, ss.Context().Err())
			return nil
		})
		require.NoError(t, err)

		err = st.interceptor()(nil, &fakeStream{ctx: t.Context()}, streamInfo, func(any, grpc.ServerStream) error {
			return status.Error(codes.Canceled, "cancelled by the handler")
		})
		require.Equal(t, codes.Canceled, status.Code(err), "only a stream Shutdown cancelled becomes Unavailable")
	})

	t.Run("leaves a stream its client cancelled first", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newStreams()
			rpc, cancelRPC := context.WithCancel(t.Context())
			err := st.interceptor()(nil, &fakeStream{ctx: rpc}, streamInfo, func(_ any, ss grpc.ServerStream) error {
				cancelRPC()
				st.stop()
				synctest.Wait()
				return ss.Context().Err()
			})
			require.ErrorIs(t, err, context.Canceled, "the client cancelled the stream, not Shutdown")
		})
	})

	t.Run("cancels a stream in flight when stopped, derived from the RPC's context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			type key struct{}
			st := newStreams()
			rpc := context.WithValue(t.Context(), key{}, "the RPC's")
			done := make(chan error, 1)
			go func() {
				done <- st.interceptor()(nil, &fakeStream{ctx: rpc}, streamInfo, func(_ any, ss grpc.ServerStream) error {
					assert.Equal(t, "the RPC's", ss.Context().Value(key{}))
					<-ss.Context().Done()
					return ss.Context().Err()
				})
			}()
			synctest.Wait()
			require.Empty(t, done, "the stream ended before stop")

			st.stop()
			err := <-done
			require.Equal(t, codes.Unavailable, status.Code(err), err)
		})
	})

	t.Run("does not make RecvMsg return", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newStreams()
			fs := &fakeStream{ctx: t.Context(), release: make(chan struct{})}
			received := make(chan error, 1)
			done := make(chan error, 1)
			go func() {
				done <- st.interceptor()(nil, fs, streamInfo, func(_ any, ss grpc.ServerStream) error {
					received <- ss.RecvMsg(nil)
					return nil
				})
			}()
			synctest.Wait()

			st.stop()
			synctest.Wait()
			require.Empty(t, received, "RecvMsg returned when the stream's context was cancelled")

			close(fs.release)
			require.EqualError(t, <-received, "released")
			require.Equal(t, codes.Unavailable, status.Code(<-done))
		})
	})
}
