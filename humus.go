// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides a base config and abstraction for running apps.
package humus

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/z5labs/humus/internal"
	"github.com/z5labs/humus/internal/global"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/pkg/app"
	"github.com/z5labs/bedrock/pkg/appbuilder"
	"github.com/z5labs/bedrock/pkg/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
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
var configBytes []byte

func init() {
	global.RegisterConfigSource(internal.ConfigSource(bytes.NewReader(configBytes)))
}

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
func Run[T any](r io.Reader, build func(context.Context, T) (App, error)) {
	cfgSrcs := global.ConfigSources
	cfgSrcs = append(cfgSrcs, internal.ConfigSource(r))

	// there's a chance Run failed on config parsing/unmarshalling
	// thus the logging config is most likely unusable and we should
	// instead create our own logger here for logging this error
	fallbackLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
	}))

	m, err := config.Read(cfgSrcs...)
	if err != nil {
		fallbackLogger.Error("failed to read config sources", slog.String("error", err.Error()))
		return
	}

	var cfg Config
	err = m.Unmarshal(&cfg)
	if err != nil {
		fallbackLogger.Error("failed to unmarshal config", slog.String("error", err.Error()))
		return
	}

	postRun := &postRun{}
	err = bedrock.Run(
		context.Background(),
		appbuilder.WithOTel(
			buildApp(build, postRun),
			appbuilder.OTelTextMapPropogator(initTextMapPropogator(cfg.OTel)),
			appbuilder.OTelTracerProvider(initTracerProvider(cfg.OTel, postRun)),
			appbuilder.OTelMeterProvider(initMeterProvider(cfg.OTel, postRun)),
			appbuilder.OTelLoggerProvider(initLogProvider(cfg.OTel, postRun)),
		),
		cfgSrcs...,
	)
	if err == nil {
		return
	}
	fallbackLogger.Error("failed while running application", slog.String("error", err.Error()))
}

type postRun struct {
	hooks []app.LifecycleHook
}

func buildApp[T any](f func(context.Context, T) (App, error), postRun *postRun) bedrock.AppBuilder[T] {
	return bedrock.AppBuilderFunc[T](func(ctx context.Context, cfg T) (bedrock.App, error) {
		ba, err := f(ctx, cfg)
		if err != nil {
			return nil, err
		}

		var base bedrock.App = ba
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

func initTracerProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (trace.TracerProvider, error) {
	return func(ctx context.Context) (trace.TracerProvider, error) {
		if cfg.OTLP.Target == "" {
			return tracenoop.NewTracerProvider(), nil
		}

		r, err := detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		exp, err := otlptracegrpc.New(
			ctx,
			otlptracegrpc.WithEndpoint(cfg.OTLP.Target),
			otlptracegrpc.WithCompressor("gzip"),
		)
		if err != nil {
			return nil, err
		}

		sampler := sdktrace.TraceIDRatioBased(cfg.Trace.Sampling)

		bsp := sdktrace.NewBatchSpanProcessor(
			exp,
			sdktrace.WithBatchTimeout(cfg.Trace.BatchTimeout),
		)

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(r),
			sdktrace.WithSampler(sampler),
			sdktrace.WithSpanProcessor(bsp),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(tp))
		return tp, nil
	}
}

func initMeterProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (metric.MeterProvider, error) {
	return func(ctx context.Context) (metric.MeterProvider, error) {
		if cfg.OTLP.Target == "" {
			return metricnoop.NewMeterProvider(), nil
		}

		r, err := detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		exp, err := otlpmetricgrpc.New(
			ctx,
			otlpmetricgrpc.WithEndpoint(cfg.OTLP.Target),
			otlpmetricgrpc.WithCompressor("gzip"),
		)
		if err != nil {
			return nil, err
		}

		reader := sdkmetric.NewPeriodicReader(
			exp,
			sdkmetric.WithInterval(cfg.Metric.ExportPeriod),
		)

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(r),
			sdkmetric.WithReader(reader),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(mp))
		return mp, nil
	}
}

func initLogProvider(cfg OTelConfig, postRun *postRun) func(context.Context) (log.LoggerProvider, error) {
	return func(ctx context.Context) (log.LoggerProvider, error) {
		r, err := detectResource(ctx, cfg)
		if err != nil {
			return nil, err
		}

		p, err := initLogProcessor(ctx, cfg)
		if err != nil {
			return nil, err
		}

		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(r),
			sdklog.WithProcessor(p),
		)
		postRun.hooks = append(postRun.hooks, shutdownHook(lp))
		return lp, nil
	}
}

func initLogProcessor(ctx context.Context, cfg OTelConfig) (sdklog.Processor, error) {
	if cfg.OTLP.Target == "" {
		exp, err := stdoutlog.New()
		if err != nil {
			return nil, err
		}

		return sdklog.NewSimpleProcessor(exp), nil
	}

	exp, err := otlploggrpc.New(
		ctx,
		otlploggrpc.WithEndpoint(cfg.OTLP.Target),
		otlploggrpc.WithCompressor("gzip"),
	)
	if err != nil {
		return nil, err
	}

	bp := sdklog.NewBatchProcessor(
		exp,
		sdklog.WithExportInterval(cfg.Log.BatchTimeout),
	)
	return bp, nil
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
