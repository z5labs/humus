// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpc

import (
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/internal/grpchealth"

	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	experimental "google.golang.org/grpc/experimental/opentelemetry"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/stats/opentelemetry"
)

// ApiOptions represents configurable value for a [Api].
type ApiOptions struct{}

// ApiOption sets value on [ApiOptions].
type ApiOption interface {
	ApplyApiOption(*ApiOptions)
}

// Api is a [grpc.ServiceRegistrar] that handles configuring
// OTel and the gRPC Health service for each service you register
// with it.
type Api struct {
	healthServer *grpchealth.Server
	server       *grpc.Server
}

// NewApi initializes a new [Api].
func NewApi(opts ...ApiOption) *Api {
	ao := &ApiOptions{}
	for _, opt := range opts {
		opt.ApplyApiOption(ao)
	}

	srv := grpc.NewServer(
		opentelemetry.ServerOption(opentelemetry.Options{
			MetricsOptions: opentelemetry.MetricsOptions{
				MeterProvider: otel.GetMeterProvider(),
				Metrics:       opentelemetry.DefaultMetrics(),
			},
			TraceOptions: experimental.TraceOptions{
				TracerProvider:    otel.GetTracerProvider(),
				TextMapPropagator: otel.GetTextMapPropagator(),
			},
		}),
	)

	healthServer := grpchealth.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthServer)

	return &Api{
		healthServer: healthServer,
		server:       srv,
	}
}

// RegisterService implements the [grpc.ServiceRegistrar] interface.
func (api *Api) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if m, ok := impl.(health.Monitor); ok {
		api.healthServer.MonitorService(desc.ServiceName, m)
	}

	api.server.RegisterService(desc, impl)
}
