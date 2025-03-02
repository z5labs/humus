// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpc

import (
	"context"
	"net"
	"testing"

	"github.com/z5labs/humus/grpc/internal/echo"
	"github.com/z5labs/humus/grpc/internal/echopb"
	"github.com/z5labs/humus/health"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type echoWithHealthMonitoring struct {
	echo.Service
	monitor health.Monitor
}

func (e echoWithHealthMonitoring) Healthy(ctx context.Context) (bool, error) {
	return e.monitor.Healthy(ctx)
}

func TestApi_Health_Check(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the requested service does not implement health.Monitor", func(t *testing.T) {
			api := NewApi()

			echopb.RegisterEchoServer(api, echo.Service{})

			ls, err := net.Listen("tcp", "127.0.0.1:0")
			if !assert.Nil(t, err) {
				return
			}

			eg, _ := errgroup.WithContext(context.Background())
			eg.Go(func() error {
				return api.server.Serve(ls)
			})
			defer eg.Wait()
			defer api.server.GracefulStop()

			cc, err := grpc.NewClient(
				ls.Addr().String(),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if !assert.Nil(t, err) {
				return
			}

			healthClient := grpc_health_v1.NewHealthClient(cc)

			_, err = healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{
				Service: echopb.Echo_ServiceDesc.ServiceName,
			})

			s, ok := status.FromError(err)
			if !assert.True(t, ok) {
				return
			}
			if !assert.Equal(t, codes.NotFound, s.Code()) {
				return
			}
		})
	})

	t.Run("will return status SERVING", func(t *testing.T) {
		t.Run("if the requested service implements health.Monitor and is healthy", func(t *testing.T) {
			api := NewApi()

			var toggle health.Binary
			toggle.MarkHealthy()

			e := echoWithHealthMonitoring{
				monitor: &toggle,
			}

			echopb.RegisterEchoServer(api, e)

			ls, err := net.Listen("tcp", "127.0.0.1:0")
			if !assert.Nil(t, err) {
				return
			}

			eg, _ := errgroup.WithContext(context.Background())
			eg.Go(func() error {
				return api.server.Serve(ls)
			})
			defer eg.Wait()
			defer api.server.GracefulStop()

			cc, err := grpc.NewClient(
				ls.Addr().String(),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if !assert.Nil(t, err) {
				return
			}

			healthClient := grpc_health_v1.NewHealthClient(cc)

			resp, err := healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{
				Service: echopb.Echo_ServiceDesc.ServiceName,
			})
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.GetStatus()) {
				return
			}
		})
	})

	t.Run("will return status NOT_SERVING", func(t *testing.T) {
		t.Run("if the requested service implements health.Monitor and is not healthy", func(t *testing.T) {
			api := NewApi()

			var toggle health.Binary
			toggle.MarkUnhealthy()

			e := echoWithHealthMonitoring{
				monitor: &toggle,
			}

			echopb.RegisterEchoServer(api, e)

			ls, err := net.Listen("tcp", "127.0.0.1:0")
			if !assert.Nil(t, err) {
				return
			}

			eg, _ := errgroup.WithContext(context.Background())
			eg.Go(func() error {
				return api.server.Serve(ls)
			})
			defer eg.Wait()
			defer api.server.GracefulStop()

			cc, err := grpc.NewClient(
				ls.Addr().String(),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if !assert.Nil(t, err) {
				return
			}

			healthClient := grpc_health_v1.NewHealthClient(cc)

			resp, err := healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{
				Service: echopb.Echo_ServiceDesc.ServiceName,
			})
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING, resp.GetStatus()) {
				return
			}
		})
	})
}
