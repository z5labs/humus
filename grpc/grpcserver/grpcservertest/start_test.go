// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcservertest_test

import (
	"context"
	"crypto/tls"
	"errors"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// stuckServer's UnaryCall closes entered, then waits for its context and
// sends when it was done on cancelled.
type stuckServer struct {
	testpb.UnimplementedTestServiceServer

	entered   chan struct{}
	cancelled chan time.Time
}

func (s *stuckServer) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	close(s.entered)
	<-ctx.Done()
	s.cancelled <- time.Now()
	return nil, ctx.Err()
}

// serves returns an Init whose only Service serves ts on ":9090", over TLS
// with cert.
func serves(cert *humustest.Certificate, ts testpb.TestServiceServer) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: ":9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, ts)
		return []humus.Service{svc}, nil
	}
}

func TestStart_applies_the_defaults_before_the_test_options(t *testing.T) {
	cases := []struct {
		name  string
		opts  []humustest.Option
		want  time.Duration
		grace time.Duration
	}{
		{
			name:  "cancel a stuck RPC after the 5s drain and the 15s grace period",
			want:  20 * time.Second,
			grace: 15 * time.Second,
		},
		{
			name:  "keep the drain under a grace period given with WithApp",
			opts:  []humustest.Option{humustest.WithApp(humus.WithShutdownGracePeriod(2 * time.Second))},
			want:  7 * time.Second,
			grace: 2 * time.Second,
		},
		{
			name:  "keep the drain when HUMUS_SHUTDOWN_GRACE_PERIOD overrides the grace period",
			opts:  []humustest.Option{humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"})},
			want:  7 * time.Second,
			grace: 2 * time.Second,
		},
		{
			name:  "keep the grace period when HUMUS_SHUTDOWN_DRAIN_DELAY overrides the drain",
			opts:  []humustest.Option{humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_DRAIN_DELAY": "1s"})},
			want:  16 * time.Second,
			grace: 15 * time.Second,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				ts := &stuckServer{entered: make(chan struct{}), cancelled: make(chan time.Time, 1)}
				opts := append([]humustest.Option{
					humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				}, c.opts...)
				p, exit := grpcservertest.Start(t, serves(ca.Issue(t, "carts"), ts), opts...)
				<-h.Listening("carts:9090")

				client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
				got := make(chan error, 1)
				go func() {
					_, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
					got <- err
				}()
				<-ts.entered
				<-p.Notified(syscall.SIGTERM)
				sigterm := time.Now()
				p.Signal(syscall.SIGTERM)

				require.Equal(t, c.want, (<-ts.cancelled).Sub(sigterm), "the RPC's context is cancelled after the drain and the grace period")
				require.Error(t, <-got, "the RPC fails")

				st := <-exit
				require.Equal(t, humus.ExitFailure, st.Code())
				gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
				require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
				require.Equal(t, c.grace, gerr.GracePeriod)
			})
		})
	}
}
