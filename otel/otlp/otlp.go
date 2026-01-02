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
	"errors"

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

// Protocol represents the transport protocol used by OTLP exporters.
type Protocol string

const (
	// ProtocolGRPC indicates the exporter should use gRPC transport.
	ProtocolGRPC Protocol = "grpc"
	// ProtocolHTTPProtobuf indicates the exporter should use HTTP with protobuf encoding.
	ProtocolHTTPProtobuf Protocol = "http/protobuf"
)

func selectProtocolExporter[T any](
	enabled config.Reader[bool],
	protocol config.Reader[Protocol],
	grpcExporter config.Reader[T],
	httpExporter config.Reader[T],
) config.Reader[T] {
	return config.Bind(enabled, func(ctx context.Context, isEnabled bool) config.Reader[T] {
		return config.Bind(protocol, func(ctx context.Context, proto Protocol) config.Reader[T] {
			if !isEnabled {
				var zero T
				return config.ReaderOf[T](zero)
			}

			switch proto {
			case ProtocolGRPC:
				return grpcExporter
			case ProtocolHTTPProtobuf:
				return httpExporter
			default:
				return config.ReaderFunc[T](func(ctx context.Context) (config.Value[T], error) {
					return config.Value[T]{}, errors.New("unsupported protocol for exporter: " + string(proto))
				})
			}
		})
	})
}

// TracesEnabledFromEnv reads the OTEL_EXPORTER_OTLP_TRACES_ENABLED environment variable.
// Returns a config.Reader[bool] that resolves to true if traces exporting is enabled.
func TracesEnabledFromEnv() config.Reader[bool] {
	return config.BoolFromString(config.Env("OTEL_EXPORTER_OTLP_TRACES_ENABLED"))
}

// TracesProtocolFromEnv reads the OTEL_EXPORTER_OTLP_TRACES_PROTOCOL environment variable.
// Returns a config.Reader[Protocol] that resolves to the configured protocol ("grpc" or "http/protobuf").
func TracesProtocolFromEnv() config.Reader[Protocol] {
	return config.Map(config.Env("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"), func(ctx context.Context, v string) (Protocol, error) {
		return Protocol(v), nil
	})
}

// SelectSpanExporter selects between gRPC and HTTP span exporters based on enabled flag and protocol.
// If enabled is false, returns a zero-value exporter. Otherwise, returns the exporter matching the protocol.
// Defaults enabled to false if not specified. Returns an error for unsupported protocols.
func SelectSpanExporter(
	enabled config.Reader[bool],
	protocol config.Reader[Protocol],
	grpcExporter config.Reader[sdktrace.SpanExporter],
	httpExporter config.Reader[sdktrace.SpanExporter],
) config.Reader[sdktrace.SpanExporter] {
	return selectProtocolExporter(
		config.Default(false, enabled),
		protocol,
		grpcExporter,
		httpExporter,
	)
}

// MetricsEnabledFromEnv reads the OTEL_EXPORTER_OTLP_METRICS_ENABLED environment variable.
// Returns a config.Reader[bool] that resolves to true if metrics exporting is enabled.
func MetricsEnabledFromEnv() config.Reader[bool] {
	return config.BoolFromString(config.Env("OTEL_EXPORTER_OTLP_METRICS_ENABLED"))
}

// MetricsProtocolFromEnv reads the OTEL_EXPORTER_OTLP_METRICS_PROTOCOL environment variable.
// Returns a config.Reader[Protocol] that resolves to the configured protocol ("grpc" or "http/protobuf").
func MetricsProtocolFromEnv() config.Reader[Protocol] {
	return config.Map(config.Env("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"), func(ctx context.Context, v string) (Protocol, error) {
		return Protocol(v), nil
	})
}

// SelectMetricExporter selects between gRPC and HTTP metric exporters based on enabled flag and protocol.
// If enabled is false, returns a zero-value exporter. Otherwise, returns the exporter matching the protocol.
// Defaults enabled to false if not specified. Returns an error for unsupported protocols.
func SelectMetricExporter(
	enabled config.Reader[bool],
	protocol config.Reader[Protocol],
	grpcExporter config.Reader[sdkmetric.Exporter],
	httpExporter config.Reader[sdkmetric.Exporter],
) config.Reader[sdkmetric.Exporter] {
	return selectProtocolExporter(
		config.Default(false, enabled),
		protocol,
		grpcExporter,
		httpExporter,
	)
}

// LogsEnabledFromEnv reads the OTEL_EXPORTER_OTLP_LOGS_ENABLED environment variable.
// Returns a config.Reader[bool] that resolves to true if logs exporting is enabled.
func LogsEnabledFromEnv() config.Reader[bool] {
	return config.BoolFromString(config.Env("OTEL_EXPORTER_OTLP_LOGS_ENABLED"))
}

// LogsProtocolFromEnv reads the OTEL_EXPORTER_OTLP_LOGS_PROTOCOL environment variable.
// Returns a config.Reader[Protocol] that resolves to the configured protocol ("grpc" or "http/protobuf").
func LogsProtocolFromEnv() config.Reader[Protocol] {
	return config.Map(config.Env("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"), func(ctx context.Context, v string) (Protocol, error) {
		return Protocol(v), nil
	})
}

// SelectLogExporter selects between gRPC and HTTP log exporters based on enabled flag and protocol.
// If enabled is false, returns a zero-value exporter. Otherwise, returns the exporter matching the protocol.
// Defaults enabled to false if not specified. Returns an error for unsupported protocols.
func SelectLogExporter(
	enabled config.Reader[bool],
	protocol config.Reader[Protocol],
	grpcExporter config.Reader[sdklog.Exporter],
	httpExporter config.Reader[sdklog.Exporter],
) config.Reader[sdklog.Exporter] {
	return selectProtocolExporter(
		config.Default(false, enabled),
		protocol,
		grpcExporter,
		httpExporter,
	)
}

// GrpcConn creates a gRPC client connection with insecure credentials.
// It implements config.Reader[*grpc.ClientConn].
type GrpcConn struct {
	Target config.Reader[string]
}

// Read creates a new gRPC client connection to the configured target using insecure credentials.
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

// GrpcTraceExporter creates an OTLP trace exporter using gRPC transport.
// It implements config.Reader[sdktrace.SpanExporter].
type GrpcTraceExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

// GrpcTraceExporterFromEnv creates a GrpcTraceExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP trace exporter using the configured gRPC connection.
func (cfg GrpcTraceExporter) Read(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdktrace.SpanExporter]{}, err
	}

	return config.ValueOf[sdktrace.SpanExporter](exp), nil
}

// GrpcMetricExporter creates an OTLP metric exporter using gRPC transport.
// It implements config.Reader[sdkmetric.Exporter].
type GrpcMetricExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

// GrpcMetricExporterFromEnv creates a GrpcMetricExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP metric exporter using the configured gRPC connection.
func (cfg GrpcMetricExporter) Read(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlpmetricgrpc.New(context.Background(), otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdkmetric.Exporter]{}, err
	}

	return config.ValueOf[sdkmetric.Exporter](exp), nil
}

// GrpcLogExporter creates an OTLP log exporter using gRPC transport.
// It implements config.Reader[sdklog.Exporter].
type GrpcLogExporter struct {
	Conn config.Reader[*grpc.ClientConn]
}

// GrpcLogExporterFromEnv creates a GrpcLogExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP log exporter using the configured gRPC connection.
func (cfg GrpcLogExporter) Read(ctx context.Context) (config.Value[sdklog.Exporter], error) {
	conn := config.Must(ctx, cfg.Conn)

	exp, err := otlploggrpc.New(context.Background(), otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return config.Value[sdklog.Exporter]{}, err
	}

	return config.ValueOf[sdklog.Exporter](exp), nil
}

// HttpTraceExporter creates an OTLP trace exporter using HTTP/protobuf transport.
// It implements config.Reader[sdktrace.SpanExporter].
type HttpTraceExporter struct {
	Endpoint config.Reader[string]
}

// HttpTraceExporterFromEnv creates an HttpTraceExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP trace exporter using HTTP/protobuf transport to the configured endpoint.
func (cfg HttpTraceExporter) Read(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdktrace.SpanExporter]{}, err
	}
	return config.ValueOf[sdktrace.SpanExporter](exp), nil
}

// HttpMetricExporter creates an OTLP metric exporter using HTTP/protobuf transport.
// It implements config.Reader[sdkmetric.Exporter].
type HttpMetricExporter struct {
	Endpoint config.Reader[string]
}

// HttpMetricExporterFromEnv creates an HttpMetricExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP metric exporter using HTTP/protobuf transport to the configured endpoint.
func (cfg HttpMetricExporter) Read(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlpmetrichttp.New(context.Background(), otlpmetrichttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdkmetric.Exporter]{}, err
	}
	return config.ValueOf[sdkmetric.Exporter](exp), nil
}

// HttpLogExporter creates an OTLP log exporter using HTTP/protobuf transport.
// It implements config.Reader[sdklog.Exporter].
type HttpLogExporter struct {
	Endpoint config.Reader[string]
}

// HttpLogExporterFromEnv creates an HttpLogExporter configured from environment variables.
// Reads endpoint from OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, falling back to OTEL_EXPORTER_OTLP_ENDPOINT.
// Optional overrides can customize the exporter configuration.
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

// Read creates an OTLP log exporter using HTTP/protobuf transport to the configured endpoint.
func (cfg HttpLogExporter) Read(ctx context.Context) (config.Value[sdklog.Exporter], error) {
	endpoint := config.Must(ctx, cfg.Endpoint)

	exp, err := otlploghttp.New(context.Background(), otlploghttp.WithEndpoint(endpoint))
	if err != nil {
		return config.Value[sdklog.Exporter]{}, err
	}
	return config.ValueOf[sdklog.Exporter](exp), nil
}
