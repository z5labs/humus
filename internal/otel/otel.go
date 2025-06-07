// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/z5labs/humus/concurrent"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/internal/detector"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func Initialize(ctx context.Context, cfg config.OTel) error {
	r, err := detectResource(ctx, cfg.Resource)
	if err != nil {
		return err
	}

	grpcCache := concurrent.NewCache[string, *grpc.ClientConn]()

	initers := []initializer{
		traceProviderInitializer{
			cfg:       cfg.Trace,
			r:         r,
			grpcCache: grpcCache,
		},
		meterProviderInitializer{
			cfg:       cfg.Metric,
			r:         r,
			grpcCache: grpcCache,
		},
		logProviderInitializer{
			cfg:       cfg.Log,
			r:         r,
			grpcCache: grpcCache,
		},
	}

	for _, initer := range initers {
		err := initer.Init(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func getOrNewClientConn(cfg config.OTLP, cache *concurrent.Cache[string, *grpc.ClientConn]) (*grpc.ClientConn, error) {
	return cache.GetOr(cfg.Target, func() (*grpc.ClientConn, error) {
		return grpc.NewClient(
			cfg.Target,
			// TODO: support secure transport credentials
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	})
}

func detectResource(ctx context.Context, cfg config.Resource) (*resource.Resource, error) {
	return resource.Detect(
		ctx,
		detector.TelemetrySDK(),
		detector.Host(),
		detector.ServiceName(cfg.ServiceName),
		detector.ServiceVersion(cfg.ServiceVersion),
	)
}

type initializer interface {
	Init(context.Context) error
}

type traceProviderInitializer struct {
	cfg       config.Trace
	r         *resource.Resource
	grpcCache *concurrent.Cache[string, *grpc.ClientConn]
}

func (tpi traceProviderInitializer) Init(ctx context.Context) error {
	exp, err := initSpanExporter(ctx, tpi.cfg.Exporter, tpi.grpcCache)
	if err != nil {
		return err
	}

	sp, err := initSpanProcessor(ctx, tpi.cfg.Processor, exp)
	if err != nil {
		return err
	}

	tp := trace.NewTracerProvider(
		trace.WithSpanProcessor(sp),
		trace.WithSampler(trace.TraceIDRatioBased(tpi.cfg.Sampling.Ratio)),
		trace.WithResource(tpi.r),
	)
	otel.SetTracerProvider(tp)
	return nil
}

type UnknownOTLPConnTypeError struct {
	Type config.OTLPConnType
}

func (e UnknownOTLPConnTypeError) Error() string {
	return fmt.Sprintf("unknown otlp conn type: %q", e.Type)
}

func initSpanExporter(ctx context.Context, cfg config.SpanExporter, grpcCache *concurrent.Cache[string, *grpc.ClientConn]) (trace.SpanExporter, error) {
	switch cfg.Type {
	case config.OTLPSpanExporterType:
		switch cfg.OTLP.Type {
		case config.OTLPGRPC:
			cc, err := getOrNewClientConn(cfg.OTLP, grpcCache)
			if err != nil {
				return nil, err
			}

			return otlptracegrpc.New(
				ctx,
				otlptracegrpc.WithGRPCConn(cc),
			)
		case config.OTLPHTTP:
			return otlptracehttp.New(
				ctx,
				otlptracehttp.WithEndpoint(cfg.OTLP.Target),
			)
		default:
			return nil, UnknownOTLPConnTypeError{
				Type: cfg.OTLP.Type,
			}
		}
	default:
		return noopSpanExporter{}, nil
	}
}

type UnknownSpanProcessorTypeError struct {
	Type config.SpanProcessorType
}

func (e UnknownSpanProcessorTypeError) Error() string {
	return fmt.Sprintf("unknown span processor type: %q", e.Type)
}

func initSpanProcessor(ctx context.Context, cfg config.SpanProcessor, exp trace.SpanExporter) (trace.SpanProcessor, error) {
	switch cfg.Type {
	case config.BatchSpanProcessorType:
		bsp := trace.NewBatchSpanProcessor(
			exp,
			trace.WithBatchTimeout(cfg.Batch.ExportInterval),
			trace.WithMaxExportBatchSize(cfg.Batch.MaxSize),
		)
		return bsp, nil
	default:
		return nil, UnknownSpanProcessorTypeError{
			Type: cfg.Type,
		}
	}
}

type meterProviderInitializer struct {
	cfg       config.Metric
	r         *resource.Resource
	grpcCache *concurrent.Cache[string, *grpc.ClientConn]
}

func (mpi meterProviderInitializer) Init(ctx context.Context) error {
	exp, err := initMetricExporter(ctx, mpi.cfg.Exporter, mpi.grpcCache)
	if err != nil {
		return err
	}

	r, err := initMetricReader(ctx, mpi.cfg.Reader, exp)
	if err != nil {
		return err
	}

	mp := metric.NewMeterProvider(
		metric.WithReader(r),
		metric.WithResource(mpi.r),
	)
	otel.SetMeterProvider(mp)

	return runtime.Start(
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	)
}

func initMetricExporter(ctx context.Context, cfg config.MetricExporter, grpcCache *concurrent.Cache[string, *grpc.ClientConn]) (metric.Exporter, error) {
	switch cfg.Type {
	case config.OTLPMetricExporterType:
		switch cfg.OTLP.Type {
		case config.OTLPGRPC:
			cc, err := getOrNewClientConn(cfg.OTLP, grpcCache)
			if err != nil {
				return nil, err
			}
			if err != nil {
				return nil, err
			}

			return otlpmetricgrpc.New(
				ctx,
				otlpmetricgrpc.WithGRPCConn(cc),
			)
		case config.OTLPHTTP:
			return otlpmetrichttp.New(
				ctx,
				otlpmetrichttp.WithEndpoint(cfg.OTLP.Target),
			)
		default:
			return nil, UnknownOTLPConnTypeError{
				Type: cfg.OTLP.Type,
			}
		}
	default:
		return noopMetricExporter{}, nil
	}
}

type UnknownMetricReaderTypeError struct {
	Type config.MetricReaderType
}

func (e UnknownMetricReaderTypeError) Error() string {
	return fmt.Sprintf("unknown metric reader type: %q", e.Type)
}

func initMetricReader(ctx context.Context, cfg config.MetricReader, exp metric.Exporter) (metric.Reader, error) {
	switch cfg.Type {
	case config.PeriodicReaderType:
		r := metric.NewPeriodicReader(
			exp,
			metric.WithInterval(cfg.Periodic.ExportInterval),
			metric.WithProducer(runtime.NewProducer()),
		)
		return r, nil
	default:
		return nil, UnknownMetricReaderTypeError{
			Type: cfg.Type,
		}
	}
}

type logProviderInitializer struct {
	cfg       config.Log
	r         *resource.Resource
	grpcCache *concurrent.Cache[string, *grpc.ClientConn]
}

func (lpi logProviderInitializer) Init(ctx context.Context) error {
	exp, err := initLogExporter(ctx, lpi.cfg.Exporter, lpi.grpcCache)
	if err != nil {
		return err
	}

	lp, err := initLogProcessor(ctx, lpi.cfg.Processor, exp)
	if err != nil {
		return err
	}

	provider := log.NewLoggerProvider(
		log.WithProcessor(lp),
		log.WithResource(lpi.r),
	)
	global.SetLoggerProvider(provider)

	return nil
}

func initLogExporter(ctx context.Context, cfg config.LogExporter, grpcCache *concurrent.Cache[string, *grpc.ClientConn]) (log.Exporter, error) {
	switch cfg.Type {
	case config.OTLPLogExporterType:
		switch cfg.OTLP.Type {
		case config.OTLPGRPC:
			cc, err := getOrNewClientConn(cfg.OTLP, grpcCache)
			if err != nil {
				return nil, err
			}
			if err != nil {
				return nil, err
			}

			return otlploggrpc.New(
				ctx,
				otlploggrpc.WithGRPCConn(cc),
			)
		case config.OTLPHTTP:
			return otlploghttp.New(
				ctx,
				otlploghttp.WithEndpoint(cfg.OTLP.Target),
			)
		default:
			return nil, UnknownOTLPConnTypeError{
				Type: cfg.OTLP.Type,
			}
		}
	default:
		exp := &slogExporter{
			handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}),
		}
		return exp, nil
	}
}

type UnknownLogProcessorTypeError struct {
	Type config.LogProcessorType
}

func (e UnknownLogProcessorTypeError) Error() string {
	return fmt.Sprintf("unknown log processor type: %q", e.Type)
}

func initLogProcessor(ctx context.Context, cfg config.LogProcessor, exp log.Exporter) (log.Processor, error) {
	switch cfg.Type {
	case config.SimpleLogProcessorType:
		lp := log.NewSimpleProcessor(exp)

		return lp, nil
	case config.BatchLogProcessorType:
		lp := log.NewBatchProcessor(
			exp,
			log.WithExportInterval(cfg.Batch.ExportInterval),
			log.WithExportMaxBatchSize(cfg.Batch.MaxSize),
		)

		return lp, nil
	default:
		return nil, UnknownLogProcessorTypeError{
			Type: cfg.Type,
		}
	}
}
