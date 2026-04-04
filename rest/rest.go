// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/z5labs/bedrock"
	bedrockconfig "github.com/z5labs/bedrock/config"
	bedrockhttp "github.com/z5labs/bedrock/runtime/http"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	bedrockotel "github.com/z5labs/bedrock/runtime/otel"
	"github.com/z5labs/bedrock/runtime/otel/noop"
	"github.com/z5labs/bedrock/runtime/otel/otlp"
	"github.com/z5labs/bedrock/runtime/otel/stdout"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Option configures the REST server.
type Option func(*options)

type options struct {
	// OpenAPI options
	title       string
	version     string
	description string
	specPath    string
	routes      []bedrockrest.Route

	// Server options
	port              bedrockconfig.Reader[int]
	readTimeout       bedrockconfig.Reader[time.Duration]
	readHeaderTimeout bedrockconfig.Reader[time.Duration]
	writeTimeout      bedrockconfig.Reader[time.Duration]
	idleTimeout       bedrockconfig.Reader[time.Duration]
	maxHeaderBytes    bedrockconfig.Reader[int]
	tlsConfig         bedrockconfig.Reader[*tls.Config]

	// OTel option
	otlpTarget bedrockconfig.Reader[string]
}

func defaultOptions() *options {
	return &options{
		title:   "API",
		version: "0.0.0",

		port: bedrockconfig.Default(
			8443,
			bedrockconfig.IntFromString(bedrockconfig.Env("HUMUS_REST_PORT")),
		),
		readTimeout: bedrockconfig.Default(
			5*time.Second,
			bedrockconfig.DurationFromString(bedrockconfig.Env("HUMUS_REST_READ_TIMEOUT")),
		),
		readHeaderTimeout: bedrockconfig.Default(
			2*time.Second,
			bedrockconfig.DurationFromString(bedrockconfig.Env("HUMUS_REST_READ_HEADER_TIMEOUT")),
		),
		writeTimeout: bedrockconfig.Default(
			10*time.Second,
			bedrockconfig.DurationFromString(bedrockconfig.Env("HUMUS_REST_WRITE_TIMEOUT")),
		),
		idleTimeout: bedrockconfig.Default(
			120*time.Second,
			bedrockconfig.DurationFromString(bedrockconfig.Env("HUMUS_REST_IDLE_TIMEOUT")),
		),
		maxHeaderBytes: bedrockconfig.Default(
			1<<20, // 1 MB
			bedrockconfig.IntFromString(bedrockconfig.Env("HUMUS_REST_MAX_HEADER_BYTES")),
		),
		tlsConfig: buildTLSConfig(
			bedrockconfig.Env("HUMUS_REST_TLS_PKCS12_FILE"),
			bedrockconfig.Env("HUMUS_REST_TLS_PKCS12_PASSWORD"),
		),
	}
}

// Title sets the OpenAPI title.
func Title(t string) Option {
	return func(o *options) {
		o.title = t
	}
}

// Version sets the OpenAPI version.
func Version(v string) Option {
	return func(o *options) {
		o.version = v
	}
}

// APIDescription sets the OpenAPI description.
func APIDescription(d string) Option {
	return func(o *options) {
		o.description = d
	}
}

// SpecPath sets the path for the OpenAPI JSON endpoint. Defaults to "/openapi.json".
func SpecPath(path string) Option {
	return func(o *options) {
		o.specPath = path
	}
}

// Handle registers a route with the REST server.
func Handle(route bedrockrest.Route) Option {
	return func(o *options) {
		o.routes = append(o.routes, route)
	}
}

// Port overrides the TCP port. The default is read from HUMUS_REST_PORT (8443).
func Port(r bedrockconfig.Reader[int]) Option {
	return func(o *options) {
		o.port = r
	}
}

// ReadTimeout overrides the request read timeout.
// The default is read from HUMUS_REST_READ_TIMEOUT (5s).
func ReadTimeout(r bedrockconfig.Reader[time.Duration]) Option {
	return func(o *options) {
		o.readTimeout = r
	}
}

// ReadHeaderTimeout overrides the request header read timeout.
// The default is read from HUMUS_REST_READ_HEADER_TIMEOUT (2s).
func ReadHeaderTimeout(r bedrockconfig.Reader[time.Duration]) Option {
	return func(o *options) {
		o.readHeaderTimeout = r
	}
}

// WriteTimeout overrides the response write timeout.
// The default is read from HUMUS_REST_WRITE_TIMEOUT (10s).
func WriteTimeout(r bedrockconfig.Reader[time.Duration]) Option {
	return func(o *options) {
		o.writeTimeout = r
	}
}

// IdleTimeout overrides the keep-alive idle timeout.
// The default is read from HUMUS_REST_IDLE_TIMEOUT (120s).
func IdleTimeout(r bedrockconfig.Reader[time.Duration]) Option {
	return func(o *options) {
		o.idleTimeout = r
	}
}

// MaxHeaderBytes overrides the maximum request header size in bytes.
// The default is read from HUMUS_REST_MAX_HEADER_BYTES (1048576).
func MaxHeaderBytes(r bedrockconfig.Reader[int]) Option {
	return func(o *options) {
		o.maxHeaderBytes = r
	}
}

// TLSConfig overrides the TLS configuration. By default, cert and key are read
// from HUMUS_REST_TLS_CERT_FILE and HUMUS_REST_TLS_KEY_FILE. If neither is set,
// a self-signed certificate is generated automatically.
func TLSConfig(r bedrockconfig.Reader[*tls.Config]) Option {
	return func(o *options) {
		o.tlsConfig = r
	}
}

// OTLPExporter configures a single OTLP gRPC destination for all three OTel
// signals (traces, metrics, logs). When set, all signals are exported to the
// given gRPC target; otherwise traces and metrics are discarded (noop) and
// logs are written to stdout.
func OTLPExporter(target bedrockconfig.Reader[string]) Option {
	return func(o *options) {
		o.otlpTarget = target
	}
}

// Run builds and runs an HTTPS REST server. It blocks until ctx is cancelled
// or a termination signal (SIGINT, SIGTERM, SIGKILL) is received.
func Run(ctx context.Context, opts ...Option) error {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	runtimeB := buildRuntime(o)

	return bedrock.NotifyOnSignal(
		bedrock.RecoverPanics(
			bedrock.DefaultRunner[bedrockotel.Runtime[
				otel.ErrorHandlerFunc,
				*sdktrace.TracerProvider,
				*sdkmetric.MeterProvider,
				*sdklog.LoggerProvider,
				bedrockhttp.Runtime,
			]](),
		),
		os.Interrupt,
		os.Kill,
		syscall.SIGTERM,
	).Run(ctx, runtimeB)
}

// buildRuntime assembles the full bedrock runtime stack.
func buildRuntime(o *options) bedrock.Builder[bedrockotel.Runtime[
	otel.ErrorHandlerFunc,
	*sdktrace.TracerProvider,
	*sdkmetric.MeterProvider,
	*sdklog.LoggerProvider,
	bedrockhttp.Runtime,
]] {
	resourceB := bedrock.MemoizeBuilder(
		bedrock.BuilderFunc[*resource.Resource](func(ctx context.Context) (*resource.Resource, error) {
			return resource.Default(), nil
		}),
	)

	tracerProviderB, meterProviderB, loggerProviderB := buildOTelProviders(o, resourceB)

	handlerB := buildHandler(o)

	listenerB := buildListener(o)

	httpRuntimeB := bedrockhttp.Build(
		listenerB,
		handlerB,
		bedrockhttp.DisableGeneralOptionsHandler(bedrockconfig.ReaderOf(false)),
		bedrockhttp.ReadTimeout(o.readTimeout),
		bedrockhttp.ReadHeaderTimeout(o.readHeaderTimeout),
		bedrockhttp.WriteTimeout(o.writeTimeout),
		bedrockhttp.IdleTimeout(o.idleTimeout),
		bedrockhttp.MaxHeaderBytes(o.maxHeaderBytes),
	)

	return bedrockotel.BuildRuntime(
		bedrock.BuilderOf(otel.ErrorHandlerFunc(func(err error) {})),
		bedrock.BuilderOf(propagation.NewCompositeTextMapPropagator(
			propagation.Baggage{},
			propagation.TraceContext{},
		)),
		tracerProviderB,
		meterProviderB,
		loggerProviderB,
		httpRuntimeB,
	)
}

// buildHandler constructs the HTTP handler with OTel instrumentation.
func buildHandler(o *options) bedrock.Builder[http.Handler] {
	apiOpts := []bedrockrest.Option{
		bedrockrest.Title(o.title),
		bedrockrest.Version(o.version),
	}
	if o.description != "" {
		apiOpts = append(apiOpts, bedrockrest.APIDescription(o.description))
	}
	if o.specPath != "" {
		apiOpts = append(apiOpts, bedrockrest.SpecPath(o.specPath))
	}
	for _, route := range o.routes {
		apiOpts = append(apiOpts, route.Route())
	}

	inner := bedrockrest.Build(apiOpts...)
	return bedrock.Map(inner, func(ctx context.Context, h http.Handler) (http.Handler, error) {
		return otelhttp.NewHandler(h, "rest"), nil
	})
}

// buildListener constructs the TLS-wrapped TCP listener.
func buildListener(o *options) bedrock.Builder[net.Listener] {
	addrReader := bedrockconfig.Map(o.port, func(_ context.Context, port int) (*net.TCPAddr, error) {
		addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return nil, err
		}
		return addr, nil
	})

	tcpListenerB := bedrockhttp.BuildTCPListener(addrReader)
	return bedrockhttp.BuildTLSListener(tcpListenerB, o.tlsConfig)
}

// buildOTelProviders builds trace, metric, and log providers using either
// OTLP gRPC exporters (when otlpTarget is set) or noop/stdout defaults.
func buildOTelProviders(
	o *options,
	resourceB bedrock.Builder[*resource.Resource],
) (
	bedrock.Builder[*sdktrace.TracerProvider],
	bedrock.Builder[*sdkmetric.MeterProvider],
	bedrock.Builder[*sdklog.LoggerProvider],
) {
	if o.otlpTarget != nil {
		return buildOTLPProviders(o.otlpTarget, resourceB)
	}
	return buildDefaultProviders(resourceB)
}

// buildDefaultProviders returns noop trace/metric providers and a stdout log provider.
func buildDefaultProviders(resourceB bedrock.Builder[*resource.Resource]) (
	bedrock.Builder[*sdktrace.TracerProvider],
	bedrock.Builder[*sdkmetric.MeterProvider],
	bedrock.Builder[*sdklog.LoggerProvider],
) {
	tracerProviderB := bedrockotel.BuildTracerProvider(
		resourceB,
		bedrockotel.BuildTraceIDRatioBasedSampler(bedrockconfig.ReaderOf(1.0)),
		bedrockotel.BuildBatchSpanProcessor(noop.BuildSpanExporter()),
	)

	meterProviderB := bedrockotel.BuildMeterProvider(
		resourceB,
		bedrockotel.BuildPeriodicReader(noop.BuildMetricExporter()),
	)

	loggerProviderB := bedrockotel.BuildLoggerProvider(
		resourceB,
		bedrockotel.BuildBatchLogProcessor(
			stdout.BuildLogExporter(bedrock.BuilderOf(os.Stdout)),
		),
	)

	return tracerProviderB, meterProviderB, loggerProviderB
}

// buildOTLPProviders returns providers backed by OTLP gRPC exporters sharing
// a single memoized gRPC connection.
func buildOTLPProviders(
	target bedrockconfig.Reader[string],
	resourceB bedrock.Builder[*resource.Resource],
) (
	bedrock.Builder[*sdktrace.TracerProvider],
	bedrock.Builder[*sdkmetric.MeterProvider],
	bedrock.Builder[*sdklog.LoggerProvider],
) {
	grpcConnB := bedrock.MemoizeBuilder(bedrock.BuilderFunc[*grpc.ClientConn](func(ctx context.Context) (*grpc.ClientConn, error) {
		addr, err := bedrockconfig.Read(ctx, target)
		if err != nil {
			return nil, err
		}
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}))

	tracerProviderB := bedrockotel.BuildTracerProvider(
		resourceB,
		bedrockotel.BuildTraceIDRatioBasedSampler(bedrockconfig.ReaderOf(1.0)),
		bedrockotel.BuildBatchSpanProcessor(otlp.BuildGrpcSpanExporter(grpcConnB)),
	)

	meterProviderB := bedrockotel.BuildMeterProvider(
		resourceB,
		bedrockotel.BuildPeriodicReader(otlp.BuildGrpcMetricExporter(grpcConnB)),
	)

	loggerProviderB := bedrockotel.BuildLoggerProvider(
		resourceB,
		bedrockotel.BuildBatchLogProcessor(otlp.BuildGrpcLogExporter(grpcConnB)),
	)

	return tracerProviderB, meterProviderB, loggerProviderB
}
