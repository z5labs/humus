// Package otlp provides OpenTelemetry Protocol (OTLP) exporters for traces, metrics, and logs.
//
// It supports both gRPC and HTTP/protobuf transport protocols, with exporters configurable
// via environment variables following the OpenTelemetry specification or programmatically
// through the config.Reader interface.
//
// # gRPC Exporters
//
// gRPC exporters use insecure credentials by default and read endpoints from environment variables:
//   - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT (traces-specific)
//   - OTEL_EXPORTER_OTLP_METRICS_ENDPOINT (metrics-specific)
//   - OTEL_EXPORTER_OTLP_LOGS_ENDPOINT (logs-specific)
//   - OTEL_EXPORTER_OTLP_ENDPOINT (fallback for all signals)
//
// # HTTP Exporters
//
// HTTP exporters use HTTP/protobuf transport and read endpoints from the same environment variables.
//
// # Example Usage
//
//	// Create a gRPC trace exporter from environment
//	exporter := otlp.GrpcTraceExporterFromEnv()
//
//	// Or with custom overrides
//	exporter := otlp.GrpcTraceExporterFromEnv(func(e *otlp.GrpcTraceExporter) {
//	    e.Conn = customConnReader
//	})
package otlp

import (
	"context"

	"github.com/z5labs/humus/config"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type GrpcConn struct {
	Target config.Reader[string]
}

func (gc *GrpcConn) Read(ctx context.Context) (config.Value[*grpc.ClientConn], error) {
	target := config.Must(ctx, gc.Target)

	cc, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return config.Value[*grpc.ClientConn]{}, err
	}

	return config.ValueOf(cc), nil
}

type GrpcTraceExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

func GrpcTraceExporterFromEnv(overrides ...func(*GrpcTraceExporter)) GrpcTraceExporter {
	exp := GrpcTraceExporter{
		Conn: &GrpcConn{
			Target: config.Or(
				config.Env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
				config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
			),
		},
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg GrpcTraceExporter) Read(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdktrace.SpanExporter]{}, err
	}

	return config.ValueOf[sdktrace.SpanExporter](exp), nil
}

type GrpcMetricExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

func GrpcMetricExporterFromEnv(overrides ...func(*GrpcMetricExporter)) GrpcMetricExporter {
	exp := GrpcMetricExporter{
		Conn: &GrpcConn{
			Target: config.Or(
				config.Env("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"),
				config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
			),
		},
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg GrpcMetricExporter) Read(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlpmetricgrpc.New(context.Background(), otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdkmetric.Exporter]{}, err
	}

	return config.ValueOf[sdkmetric.Exporter](exp), nil
}

type GrpcLogExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

func GrpcLogExporterFromEnv(overrides ...func(*GrpcLogExporter)) GrpcLogExporter {
	exp := GrpcLogExporter{
		Conn: &GrpcConn{
			Target: config.Or(
				config.Env("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"),
				config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
			),
		},
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg GrpcLogExporter) Read(ctx context.Context) (config.Value[sdklog.Exporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlploggrpc.New(context.Background(), otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdklog.Exporter]{}, err
	}

	return config.ValueOf[sdklog.Exporter](exp), nil
}

type HttpTraceExporter struct {
	Endpoint config.Reader[string]
}

func HttpTraceExporterFromEnv(overrides ...func(*HttpTraceExporter)) HttpTraceExporter {
	exp := HttpTraceExporter{
		Endpoint: config.Or(
			config.Env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
			config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
		),
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg HttpTraceExporter) Read(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdktrace.SpanExporter]{}, err
	}
	return config.ValueOf[sdktrace.SpanExporter](exp), nil
}

type HttpMetricExporter struct {
	Endpoint config.Reader[string]
}

func HttpMetricExporterFromEnv(overrides ...func(*HttpMetricExporter)) HttpMetricExporter {
	exp := HttpMetricExporter{
		Endpoint: config.Or(
			config.Env("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"),
			config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
		),
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg HttpMetricExporter) Read(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlpmetrichttp.New(context.Background(), otlpmetrichttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdkmetric.Exporter]{}, err
	}
	return config.ValueOf[sdkmetric.Exporter](exp), nil
}

type HttpLogExporter struct {
	Endpoint config.Reader[string]
}

func HttpLogExporterFromEnv(overrides ...func(*HttpLogExporter)) HttpLogExporter {
	exp := HttpLogExporter{
		Endpoint: config.Or(
			config.Env("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"),
			config.Env("OTEL_EXPORTER_OTLP_ENDPOINT"),
		),
	}
	for _, o := range overrides {
		o(&exp)
	}
	return exp
}

func (cfg HttpLogExporter) Read(ctx context.Context) (config.Value[sdklog.Exporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlploghttp.New(context.Background(), otlploghttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdklog.Exporter]{}, err
	}
	return config.ValueOf[sdklog.Exporter](exp), nil
}
