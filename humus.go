// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides a base config and abstraction for running apps.
package humus

import (
	"context"
	_ "embed"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"google.golang.org/grpc"
)

//go:embed default_config.yaml
var DefaultConfig []byte

// OTelConfig
type OTelConfig struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`

	Trace OTelTraceConfig `config:"trace"`

	Metric OTelMetricConfig `config:"metric"`

	Log OTelLogConfig `config:"log"`

	OTLP struct {
		Enabled bool   `config:"enabled"`
		Target  string `config:"target"`
	} `config:"otlp"`
}

type OTelTraceConfig struct {
	Enabled      bool          `config:"enabled"`
	Sampling     float64       `config:"sampling"`
	BatchTimeout time.Duration `config:"batch_timeout"`
}

type OTelMetricConfig struct {
	Enabled        bool          `config:"enabled"`
	ExportInterval time.Duration `config:"export_interval"`
}

type OTelLogConfig struct {
	Enabled      bool          `config:"enabled"`
	BatchTimeout time.Duration `config:"batch_timeout"`
}

// InitializeOTel implements the [appbuilder.OTelInitializer] interface.
func (oc OTelConfig) InitializeOTel(ctx context.Context) (err error) {
	var conn *grpc.ClientConn
	if oc.OTLP.Enabled {
		conn, err = grpc.NewClient(oc.OTLP.Target)
		if err != nil {
			return err
		}
	}

	r, err := resource.Detect(
		ctx,
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceNameKey, func() (string, error) {
			return oc.ServiceName, nil
		}),
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceVersionKey, func() (string, error) {
			return oc.ServiceVersion, nil
		}),
	)
	if err != nil {
		return err
	}

	fs := []func(context.Context, *grpc.ClientConn, *resource.Resource) error{
		initTracing(oc.Trace),
		initMetering(oc.Metric),
		initLogging(oc.Log),
	}

	for _, f := range fs {
		err := f(ctx, conn, r)
		if err != nil {
			return err
		}
	}
	return nil
}

func initTracing(otc OTelTraceConfig) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
	return func(ctx context.Context, cc *grpc.ClientConn, r *resource.Resource) error {
		if !otc.Enabled && cc != nil {
			return nil
		}

		exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(cc))
		if err != nil {
			return err
		}

		bsp := trace.NewBatchSpanProcessor(
			exp,
			trace.WithBatchTimeout(otc.BatchTimeout),
		)

		tp := trace.NewTracerProvider(
			trace.WithSpanProcessor(bsp),
			trace.WithSampler(trace.TraceIDRatioBased(otc.Sampling)),
			trace.WithResource(r),
		)
		otel.SetTracerProvider(tp)
		return nil
	}
}

func initMetering(omc OTelMetricConfig) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
	return func(ctx context.Context, cc *grpc.ClientConn, r *resource.Resource) error {
		if !omc.Enabled && cc != nil {
			return nil
		}

		exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(cc))
		if err != nil {
			return err
		}

		pr := metric.NewPeriodicReader(
			exp,
			metric.WithInterval(omc.ExportInterval),
			metric.WithProducer(runtime.NewProducer()),
		)

		mp := metric.NewMeterProvider(
			metric.WithReader(pr),
			metric.WithResource(r),
		)
		otel.SetMeterProvider(mp)

		return runtime.Start(
			runtime.WithMinimumReadMemStatsInterval(time.Second),
		)
	}
}

func initLogging(olc OTelLogConfig) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
	return func(ctx context.Context, cc *grpc.ClientConn, r *resource.Resource) error {
		p, err := initLogProcessor(ctx, olc, cc)
		if err != nil {
			return err
		}

		lp := log.NewLoggerProvider(
			log.WithProcessor(p),
			log.WithResource(r),
		)
		global.SetLoggerProvider(lp)
		return nil
	}
}

func initLogProcessor(ctx context.Context, olc OTelLogConfig, cc *grpc.ClientConn) (log.Processor, error) {
	// TODO: this needs to be made more specific it should either always be OTLP or STDOUT
	//		 the enabled config is a bit confusing to interpret
	if !olc.Enabled || cc == nil {
		exp, err := stdoutlog.New()
		if err != nil {
			return nil, err
		}

		sp := log.NewSimpleProcessor(exp)
		return sp, nil
	}

	exp, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(cc))
	if err != nil {
		return nil, err
	}

	bsp := log.NewBatchProcessor(exp)
	return bsp, nil
}

// LoggingConfig
type LoggingConfig struct {
	Level slog.Level `config:"level"`
}

// Config
type Config struct {
	OTelConfig    `config:"otel"`
	LoggingConfig `config:"logging"`
}

// Logger
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}
