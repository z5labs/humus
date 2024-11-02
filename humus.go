// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides a base config and abstraction for running apps.
package humus

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/pkg/app"
	"github.com/z5labs/bedrock/pkg/appbuilder"
	"github.com/z5labs/bedrock/pkg/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

//go:embed default_config.yaml
var DefaultConfig []byte

type OTelConfig struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`

	Trace struct {
		Sampling     float64       `config:"sampling"`
		BatchTimeout time.Duration `config:"batch_timeout"`
	} `config:"trace"`

	Metric struct {
		ExportPeriod time.Duration `config:"export_period"`
	} `config:"metric"`

	Log struct {
		BatchTimeout time.Duration `config:"batch_timeout"`
	} `config:"log"`

	OTLP struct {
		Target string `config:"target"`
	} `config:"otlp"`
}

type LoggingConfig struct {
	Level slog.Level `config:"level"`
}

type Config struct {
	OTel    OTelConfig    `config:"otel"`
	Logging LoggingConfig `config:"logging"`
}

// Logger
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}

// App
type App bedrock.App

// Run
func Run[T any](build func(context.Context, T) (App, error), srcs ...config.Source) error {
	runner := runner{
		srcs:           srcs,
		detectResource: detectResource,
		newTraceExporter: func(ctx context.Context, oc OTelConfig) (sdktrace.SpanExporter, error) {
			return otlptracegrpc.New(
				ctx,
				otlptracegrpc.WithEndpoint(oc.OTLP.Target),
				otlptracegrpc.WithCompressor("gzip"),
			)
		},
		newMetricExporter: func(ctx context.Context, oc OTelConfig) (sdkmetric.Exporter, error) {
			return otlpmetricgrpc.New(
				ctx,
				otlpmetricgrpc.WithEndpoint(oc.OTLP.Target),
				otlpmetricgrpc.WithCompressor("gzip"),
			)
		},
		newLogExporter: func(ctx context.Context, oc OTelConfig) (sdklog.Exporter, error) {
			if oc.OTLP.Target == "" {
				return stdoutlog.New(stdoutlog.WithWriter(os.Stdout))
			}

			return otlploggrpc.New(
				ctx,
				otlploggrpc.WithEndpoint(oc.OTLP.Target),
				otlploggrpc.WithCompressor("gzip"),
			)
		},
	}

	return run(runner, build)
}

type runner struct {
	srcs    []config.Source
	postRun postRun

	detectResource    func(context.Context, OTelConfig) (*resource.Resource, error)
	newTraceExporter  func(context.Context, OTelConfig) (sdktrace.SpanExporter, error)
	newMetricExporter func(context.Context, OTelConfig) (sdkmetric.Exporter, error)
	newLogExporter    func(context.Context, OTelConfig) (sdklog.Exporter, error)
}

func run[T any](r runner, build func(context.Context, T) (App, error)) error {
	m, err := config.Read(r.srcs...)
	if err != nil {
		return err
	}

	var cfg Config
	err = m.Unmarshal(&cfg)
	if err != nil {
		return err
	}

	return bedrock.Run(
		context.Background(),
		appbuilder.WithOTel(
			appbuilder.Recover(
				buildApp(build, &r.postRun),
			),
			appbuilder.OTelTextMapPropogator(initTextMapPropogator(cfg.OTel)),
			appbuilder.OTelTracerProvider(r.initTracerProvider(cfg.OTel, &r.postRun)),
			appbuilder.OTelMeterProvider(r.initMeterProvider(cfg.OTel, &r.postRun)),
			appbuilder.OTelLoggerProvider(r.initLoggerProvider(cfg.OTel, &r.postRun)),
		),
		m,
	)
}

type postRun struct {
	hooks []app.LifecycleHook
}

func buildApp[T any](f func(context.Context, T) (App, error), postRun *postRun) bedrock.AppBuilder[T] {
	return bedrock.AppBuilderFunc[T](func(ctx context.Context, cfg T) (bedrock.App, error) {
		spanCtx, span := otel.Tracer("humus").Start(ctx, "buildApp.Build")
		defer span.End()

		ba, err := f(spanCtx, cfg)
		if err != nil {
			return nil, err
		}

		var base bedrock.App = ba
		base = app.Recover(base)
		base = app.WithLifecycleHooks(base, app.Lifecycle{
			PostRun: composeLifecycleHooks(postRun.hooks...),
		})
		base = app.WithSignalNotifications(base, os.Interrupt, os.Kill)
		return base, nil
	})
}

func composeLifecycleHooks(hooks ...app.LifecycleHook) app.LifecycleHook {
	return app.LifecycleHookFunc(func(ctx context.Context) error {
		var hookErrs []error
		for _, hook := range hooks {
			err := hook.Run(ctx)
			if err != nil {
				hookErrs = append(hookErrs, err)
			}
		}
		if len(hookErrs) == 0 {
			return nil
		}
		return errors.Join(hookErrs...)
	})
}

func initTextMapPropogator(_ OTelConfig) func(context.Context) (propagation.TextMapPropagator, error) {
	return func(ctx context.Context) (propagation.TextMapPropagator, error) {
		tmp := propagation.NewCompositeTextMapPropagator(
			propagation.Baggage{},
			propagation.TraceContext{},
		)
		return tmp, nil
	}
}

func (r runner) initTracerProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (trace.TracerProvider, error) {
	return func(ctx context.Context) (trace.TracerProvider, error) {
		if cfg.OTLP.Target == "" {
			return tracenoop.NewTracerProvider(), nil
		}

		rsc, err := r.detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		exp, err := r.newTraceExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}

		sampler := sdktrace.TraceIDRatioBased(cfg.Trace.Sampling)

		bsp := sdktrace.NewBatchSpanProcessor(
			exp,
			sdktrace.WithBatchTimeout(cfg.Trace.BatchTimeout),
		)

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(rsc),
			sdktrace.WithSampler(sampler),
			sdktrace.WithSpanProcessor(bsp),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(tp))
		return tp, nil
	}
}

func (r runner) initMeterProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (metric.MeterProvider, error) {
	return func(ctx context.Context) (metric.MeterProvider, error) {
		if cfg.OTLP.Target == "" {
			return metricnoop.NewMeterProvider(), nil
		}

		rsc, err := r.detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		exp, err := r.newMetricExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}

		reader := sdkmetric.NewPeriodicReader(
			exp,
			sdkmetric.WithInterval(cfg.Metric.ExportPeriod),
		)

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(rsc),
			sdkmetric.WithReader(reader),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(mp))
		return mp, nil
	}
}

func (r runner) initLoggerProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (log.LoggerProvider, error) {
	return func(ctx context.Context) (log.LoggerProvider, error) {
		rsc, err := r.detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		exp, err := r.newLogExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}

		p := sdklog.NewBatchProcessor(
			exp,
			sdklog.WithExportInterval(cfg.Log.BatchTimeout),
		)

		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(rsc),
			sdklog.WithProcessor(p),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(lp))
		return lp, nil
	}
}

type resourceDetectorFunc func(context.Context) (*resource.Resource, error)

func (f resourceDetectorFunc) Detect(ctx context.Context) (*resource.Resource, error) {
	return f(ctx)
}

func detectResource(ctx context.Context, cfg OTelConfig) (*resource.Resource, error) {
	return resource.Detect(
		ctx,
		resourceDetectorFunc(func(ctx context.Context) (*resource.Resource, error) {
			return resource.Default(), nil
		}),
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceNameKey, func() (string, error) {
			return cfg.ServiceName, nil
		}),
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceVersionKey, func() (string, error) {
			return cfg.ServiceVersion, nil
		}),
	)
}

type shutdowner interface {
	Shutdown(context.Context) error
}

func shutdownHook(s shutdowner) app.LifecycleHook {
	return app.LifecycleHookFunc(func(ctx context.Context) error {
		return s.Shutdown(ctx)
	})
}
