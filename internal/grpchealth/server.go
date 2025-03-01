// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type Server struct {
	grpc_health_v1.UnimplementedHealthServer

	log               *slog.Logger
	mu                sync.Mutex
	services          map[string]health.Monitor
	watchPollInterval time.Duration
}

func NewServer() *Server {
	return &Server{
		log:               humus.Logger("grpchealth"),
		services:          make(map[string]health.Monitor),
		watchPollInterval: 10 * time.Second,
	}
}

func (s *Server) MonitorService(name string, m health.Monitor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[name] = m
}

func (s *Server) getMonitor(name string) (health.Monitor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.services[name]
	return m, ok
}

func (s *Server) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	m, ok := s.getMonitor(req.GetService())
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown service")
	}

	healthStatus := s.checkStatus(ctx, m)
	resp := &grpc_health_v1.HealthCheckResponse{
		Status: healthStatus,
	}
	return resp, nil
}

func (s *Server) checkStatus(ctx context.Context, m health.Monitor) grpc_health_v1.HealthCheckResponse_ServingStatus {
	healthy, err := m.Healthy(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to get health status from monitor", slog.Any("error", err))
		return grpc_health_v1.HealthCheckResponse_SERVICE_UNKNOWN
	}
	if healthy {
		return grpc_health_v1.HealthCheckResponse_SERVING
	}
	return grpc_health_v1.HealthCheckResponse_NOT_SERVING
}

func (s *Server) Watch(req *grpc_health_v1.HealthCheckRequest, stream grpc.ServerStreamingServer[grpc_health_v1.HealthCheckResponse]) error {
	m, ok := s.getMonitor(req.GetService())
	if !ok {
		return status.Error(codes.NotFound, "unknown service")
	}

	var healthStatus grpc_health_v1.HealthCheckResponse_ServingStatus
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return status.Error(codes.Canceled, "Stream has ended.")
		case <-time.After(s.watchPollInterval):
		}

		newHealthStatus := s.checkStatus(ctx, m)
		if newHealthStatus == healthStatus {
			continue
		}
		healthStatus = newHealthStatus

		err := stream.Send(&grpc_health_v1.HealthCheckResponse{
			Status: healthStatus,
		})
		if err != nil {
			return status.Error(codes.Canceled, "Stream has ended.")
		}
	}
}
