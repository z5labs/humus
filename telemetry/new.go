// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// The SDK settings Settings does not cover. Each one is passed to the SDK
// explicitly, at the SDK's own default, because the SDK reads an
// environment variable for any option it is not given, and humus reads no
// environment variables.
const (
	// The batch span processor's: OTEL_BSP_*.
	bspScheduleDelay   = 5 * time.Second
	bspExportTimeout   = 30 * time.Second
	bspMaxQueueSize    = 2048
	bspMaxExportBatch  = 512
	blrpScheduleDelay  = time.Second
	blrpExportTimeout  = 30 * time.Second
	blrpMaxQueueSize   = 2048
	blrpMaxExportBatch = 512

	// The periodic metric reader's: OTEL_METRIC_EXPORT_*.
	metricExportInterval = time.Minute
	metricExportTimeout  = 30 * time.Second

	// OTEL_GO_X_CARDINALITY_LIMIT.
	metricCardinalityLimit = 2000

	// The log record limits: OTEL_LOGRECORD_ATTRIBUTE_*.
	logAttributeCountLimit       = 128
	logAttributeValueLengthLimit = -1
)

// spanLimits are the SDK's default span limits, given explicitly so that
// OTEL_SPAN_*, OTEL_EVENT_* and OTEL_LINK_* are not read.
var spanLimits = sdktrace.SpanLimits{
	AttributeValueLengthLimit:   sdktrace.DefaultAttributeValueLengthLimit,
	AttributeValueDepthLimit:    sdktrace.DefaultAttributeValueDepthLimit,
	AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
	EventCountLimit:             sdktrace.DefaultEventCountLimit,
	LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
	AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
	AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
}

// The paths the OTLP specification gives each signal over HTTP.
const (
	tracesPath  = "/v1/traces"
	metricsPath = "/v1/metrics"
	logsPath    = "/v1/logs"
)

// New builds an SDK from s.
//
// With s.Disabled, New ignores every other field and returns Noop(),
// building and dialling nothing. Otherwise it runs every check, and when any
// fails it reports every bad field in one error, naming each one, and builds
// nothing.
//
// Each signal is built as its field says:
//
//   - traces: at ExporterNone, the SDK's TracerProvider with s.Sampler and
//     the resource but no span processor, so spans get real IDs and nothing
//     is exported; at ExporterConsole, OpenTelemetry's stdouttrace; at
//     ExporterOTLP, the OTLP exporter in a batch span processor.
//   - metrics: at ExporterNone, a no-op MeterProvider; at ExporterConsole,
//     OpenTelemetry's stdoutmetric; at ExporterOTLP, the OTLP exporter in a
//     periodic reader.
//   - logs: at ExporterNone, a no-op LoggerProvider; at ExporterConsole,
//     JSON lines in the format the package doc specifies; at ExporterOTLP,
//     the OTLP exporter in a batch processor. Records below s.LogLevel are
//     dropped either way.
//
// The propagator is W3C trace context plus baggage.
//
// The console exporters share stdout and their writes to it are
// serialized, so stdout need not be safe for concurrent use. The OTLP
// exporters connect through dial, and share one connection, or one HTTP
// client, between them; dial is called only once something is exported.
//
// The resource is built from s.ResourceOptions, and then service.name is
// set to s.ServiceName, so it wins over anything a detector finds. If a
// resource option fails, even partly, New fails.
//
// New reads nothing from the environment. The OpenTelemetry SDK reads an
// environment variable for each option it is not given, so New gives it
// every one, at the SDK's default where Settings has no field: the sampler,
// the span and log record limits, the batch processors' and periodic
// reader's settings, the OTLP endpoint, headers, TLS, compression and
// timeout, and the resource. The SDK also merges OTEL_RESOURCE_ATTRIBUTES
// into a resource it is given; New takes those attributes out again before
// anything is exported. What still reads the environment is OpenTelemetry's
// experimental OTEL_GO_X_* features, its internal logger, and any resource
// detector s.ResourceOptions opts in to.
func New(ctx context.Context, s Settings, dial Dialer, stdout io.Writer) (SDK, error) {
	if s.Disabled {
		return Noop(), nil
	}
	if err := s.check(); err != nil {
		return SDK{}, err
	}
	uses := func(e Exporter) bool { return s.Traces == e || s.Metrics == e || s.Logs == e }
	if dial == nil && uses(ExporterOTLP) {
		return SDK{}, errors.New("telemetry: New: dial is nil, and a signal is exported over OTLP")
	}
	if stdout == nil && uses(ExporterConsole) {
		return SDK{}, errors.New("telemetry: New: stdout is nil, and a signal is written to the console")
	}

	res, err := newResource(ctx, s)
	if err != nil {
		return SDK{}, fmt.Errorf("telemetry: building the resource: %w", err)
	}

	b := &builder{
		s:      s,
		dial:   dial,
		stdout: &syncWriter{w: stdout},
		res:    res,
	}
	sdk, err := b.build(ctx)
	if err != nil {
		// Release whatever was built before the failure. Nothing has been
		// emitted yet, so there is nothing to flush.
		_ = b.shutdown(ctx)
		return SDK{}, err
	}
	return sdk, nil
}

// newResource builds the resource from s.ResourceOptions, then sets
// service.name, so that it wins.
func newResource(ctx context.Context, s Settings) (*resource.Resource, error) {
	opts := append(slices.Clone(s.ResourceOptions), resource.WithAttributes(semconv.ServiceName(s.ServiceName)))
	return resource.New(ctx, opts...)
}

// builder holds what New builds, so that it can be shut down whether New
// finished or not.
type builder struct {
	s      Settings
	dial   Dialer
	stdout io.Writer
	res    *resource.Resource

	// Built only when a signal is exported over OTLP, so that nothing is
	// dialled otherwise.
	conn      *grpc.ClientConn
	transport *http.Transport
	client    *http.Client

	// providers are shut down concurrently, so that one that does not
	// return by the deadline does not take another's time.
	providers []func(context.Context) error
}

func (b *builder) build(ctx context.Context) (SDK, error) {
	tp, err := b.tracerProvider(ctx)
	if err != nil {
		return SDK{}, fmt.Errorf("telemetry: building traces: %w", err)
	}
	mp, err := b.meterProvider(ctx)
	if err != nil {
		return SDK{}, fmt.Errorf("telemetry: building metrics: %w", err)
	}
	lp, err := b.loggerProvider(ctx)
	if err != nil {
		return SDK{}, fmt.Errorf("telemetry: building logs: %w", err)
	}
	return SDK{
		TracerProvider: tp,
		MeterProvider:  mp,
		LoggerProvider: lp,
		Propagator:     newPropagator(),
		Shutdown:       b.shutdown,
	}, nil
}

// shutdown flushes and stops every provider within s.ShutdownTimeout, then
// releases the OTLP connections, and returns every error, joined.
func (b *builder) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.s.ShutdownTimeout)
	defer cancel()

	errs := make([]error, len(b.providers), len(b.providers)+1)
	var wg sync.WaitGroup
	for i, shutdown := range b.providers {
		wg.Go(func() { errs[i] = shutdown(ctx) })
	}
	wg.Wait()

	if b.conn != nil {
		errs = append(errs, b.conn.Close())
	}
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	return errors.Join(errs...)
}

func (b *builder) tracerProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(b.s.Sampler),
		sdktrace.WithRawSpanLimits(spanLimits),
		sdktrace.WithResource(b.res),
	}
	switch b.s.Traces {
	case ExporterConsole:
		exp, err := stdouttrace.New(stdouttrace.WithWriter(b.stdout))
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(b.spanExporter(exp))))
	case ExporterOTLP:
		exp, err := b.otlpSpanExporter(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(
			b.spanExporter(exp),
			sdktrace.WithBatchTimeout(bspScheduleDelay),
			sdktrace.WithExportTimeout(bspExportTimeout),
			sdktrace.WithMaxQueueSize(bspMaxQueueSize),
			sdktrace.WithMaxExportBatchSize(bspMaxExportBatch),
		)))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	b.providers = append(b.providers, tp.Shutdown)
	return tp, nil
}

func (b *builder) otlpSpanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if b.s.OTLP.Protocol == ProtocolHTTPProtobuf {
		return otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(b.url(tracesPath)),
			otlptracehttp.WithTLSClientConfig(b.s.OTLP.TLS),
			otlptracehttp.WithHTTPClient(b.httpClient()),
			otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
			otlptracehttp.WithHeaders(b.s.OTLP.Headers),
			otlptracehttp.WithTimeout(b.s.OTLP.Timeout),
			otlptracehttp.WithCompression(otlptracehttp.NoCompression),
		)
	}
	conn, err := b.grpcConn()
	if err != nil {
		return nil, err
	}
	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
		otlptracegrpc.WithHeaders(b.s.OTLP.Headers),
		otlptracegrpc.WithTimeout(b.s.OTLP.Timeout),
		otlptracegrpc.WithCompressor("none"),
	)
}

func (b *builder) meterProvider(ctx context.Context) (metric.MeterProvider, error) {
	var exp sdkmetric.Exporter
	var err error
	switch b.s.Metrics {
	case ExporterNone:
		return metricnoop.NewMeterProvider(), nil
	case ExporterConsole:
		exp, err = stdoutmetric.New(
			stdoutmetric.WithWriter(b.stdout),
			stdoutmetric.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
			stdoutmetric.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
		)
	case ExporterOTLP:
		exp, err = b.otlpMetricExporter(ctx)
	}
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(b.res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(
			resourceMetricExporter{Exporter: exp, res: b.res},
			sdkmetric.WithInterval(metricExportInterval),
			sdkmetric.WithTimeout(metricExportTimeout),
		)),
		sdkmetric.WithExemplarFilter(exemplar.TraceBasedFilter),
		sdkmetric.WithCardinalityLimit(metricCardinalityLimit),
	)
	b.providers = append(b.providers, mp.Shutdown)
	return mp, nil
}

func (b *builder) otlpMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	if b.s.OTLP.Protocol == ProtocolHTTPProtobuf {
		return otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(b.url(metricsPath)),
			otlpmetrichttp.WithTLSClientConfig(b.s.OTLP.TLS),
			otlpmetrichttp.WithHTTPClient(b.httpClient()),
			otlpmetrichttp.WithHeaders(b.s.OTLP.Headers),
			otlpmetrichttp.WithTimeout(b.s.OTLP.Timeout),
			otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression),
			otlpmetrichttp.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
			otlpmetrichttp.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
		)
	}
	conn, err := b.grpcConn()
	if err != nil {
		return nil, err
	}
	return otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(conn),
		otlpmetricgrpc.WithHeaders(b.s.OTLP.Headers),
		otlpmetricgrpc.WithTimeout(b.s.OTLP.Timeout),
		otlpmetricgrpc.WithCompressor("none"),
		otlpmetricgrpc.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
		otlpmetricgrpc.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
	)
}

func (b *builder) loggerProvider(ctx context.Context) (log.LoggerProvider, error) {
	var proc sdklog.Processor
	switch b.s.Logs {
	case ExporterNone:
		return lognoop.NewLoggerProvider(), nil
	case ExporterConsole:
		proc = sdklog.NewSimpleProcessor(newJSONLogExporter(b.stdout))
	case ExporterOTLP:
		exp, err := b.otlpLogExporter(ctx)
		if err != nil {
			return nil, err
		}
		proc = sdklog.NewBatchProcessor(exp,
			sdklog.WithExportInterval(blrpScheduleDelay),
			sdklog.WithExportTimeout(blrpExportTimeout),
			sdklog.WithMaxQueueSize(blrpMaxQueueSize),
			sdklog.WithExportMaxBatchSize(blrpMaxExportBatch),
		)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(b.res),
		sdklog.WithProcessor(severityFilter{Processor: proc, min: b.s.LogLevel}),
		sdklog.WithAttributeCountLimit(logAttributeCountLimit),
		sdklog.WithAttributeValueLengthLimit(logAttributeValueLengthLimit),
	)
	b.providers = append(b.providers, lp.Shutdown)
	return lp, nil
}

func (b *builder) otlpLogExporter(ctx context.Context) (sdklog.Exporter, error) {
	if b.s.OTLP.Protocol == ProtocolHTTPProtobuf {
		return otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(b.url(logsPath)),
			otlploghttp.WithTLSClientConfig(b.s.OTLP.TLS),
			otlploghttp.WithHTTPClient(b.httpClient()),
			otlploghttp.WithHeaders(b.s.OTLP.Headers),
			otlploghttp.WithTimeout(b.s.OTLP.Timeout),
			otlploghttp.WithCompression(otlploghttp.NoCompression),
		)
	}
	conn, err := b.grpcConn()
	if err != nil {
		return nil, err
	}
	return otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(conn),
		otlploggrpc.WithHeaders(b.s.OTLP.Headers),
		otlploggrpc.WithTimeout(b.s.OTLP.Timeout),
		otlploggrpc.WithCompressor("none"),
	)
}

// spanExporter wraps exp so that the spans it exports carry the resource
// New built, and not the one the SDK merged OTEL_RESOURCE_ATTRIBUTES into.
func (b *builder) spanExporter(exp sdktrace.SpanExporter) sdktrace.SpanExporter {
	return resourceSpanExporter{SpanExporter: exp, res: b.res}
}

// url is the OTLP/HTTP URL of the signal at path.
func (b *builder) url(path string) string {
	scheme := "http"
	if b.s.OTLP.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + b.s.OTLP.Endpoint + path
}

// grpcConn returns the gRPC connection the OTLP exporters share, making it
// on the first call. grpc.NewClient does not dial: the first export does.
//
// The passthrough resolver hands the endpoint to dial as it is, so that a
// name is resolved by the Host, not by DNS, and no proxy is used, so that
// none is read from the environment.
func (b *builder) grpcConn() (*grpc.ClientConn, error) {
	if b.conn != nil {
		return b.conn, nil
	}
	creds := insecure.NewCredentials()
	if b.s.OTLP.TLS != nil {
		creds = credentials.NewTLS(b.s.OTLP.TLS)
	}
	conn, err := grpc.NewClient("passthrough:///"+b.s.OTLP.Endpoint,
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return b.dial(ctx, "tcp", addr)
		}),
		grpc.WithTransportCredentials(creds),
		grpc.WithNoProxy(),
		grpc.WithUnaryInterceptor(newResourceFilter(b.res).unaryInterceptor),
	)
	if err != nil {
		return nil, err
	}
	b.conn = conn
	return conn, nil
}

// httpClient returns the HTTP client the OTLP exporters share, making it on
// the first call. Its transport dials through dial and uses no proxy, so
// that none is read from the environment.
func (b *builder) httpClient() *http.Client {
	if b.client != nil {
		return b.client
	}
	b.transport = &http.Transport{
		DialContext:     b.dial,
		TLSClientConfig: b.s.OTLP.TLS.Clone(),
	}
	b.client = &http.Client{Transport: &resourceFilterTransport{
		base:   b.transport,
		filter: newResourceFilter(b.res),
	}}
	return b.client
}

// syncWriter serializes the writes of the console exporters, which share
// one writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// severityFilter drops the records below min, and reports its loggers
// disabled below it, so that a bridge which asks Enabled first skips a
// filtered record at no cost.
//
// A record with no severity is kept: the OpenTelemetry specification's
// minimum severity affects only records whose severity is set.
type severityFilter struct {
	sdklog.Processor
	min log.Severity
}

func (f severityFilter) below(s log.Severity) bool {
	return s != log.SeverityUndefined && s < f.min
}

func (f severityFilter) Enabled(ctx context.Context, p sdklog.EnabledParameters) bool {
	if f.below(p.Severity) {
		return false
	}
	return f.Processor.Enabled(ctx, p)
}

func (f severityFilter) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if f.below(r.Severity()) {
		return nil
	}
	return f.Processor.OnEmit(ctx, r)
}
