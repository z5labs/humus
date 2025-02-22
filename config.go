// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"bytes"
	"context"
	_ "embed"
	"time"

	"github.com/z5labs/humus/config"

	bedrockcfg "github.com/z5labs/bedrock/config"
	"github.com/z5labs/humus/internal"
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
var defaultConfig []byte

// DefaultConfig
func DefaultConfig() bedrockcfg.Source {
	return internal.ConfigSource(bytes.NewReader(defaultConfig))
}

// Config
type Config struct {
	OTel config.OTel `config:"otel"`
}

// InitializeOTel implements the [appbuilder.OTelInitializer] interface.
func (cfg Config) InitializeOTel(ctx context.Context) (err error) {
	var conn *grpc.ClientConn
	if cfg.OTel.OTLP.Enabled {
		conn, err = grpc.NewClient(cfg.OTel.OTLP.Target)
		if err != nil {
			return err
		}
	}

	r, err := resource.Detect(
		ctx,
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceNameKey, func() (string, error) {
			return cfg.OTel.Resource.ServiceName, nil
		}),
		resource.StringDetector(semconv.SchemaURL, semconv.ServiceVersionKey, func() (string, error) {
			return cfg.OTel.Resource.ServiceVersion, nil
		}),
	)
	if err != nil {
		return err
	}

	fs := []func(context.Context, *grpc.ClientConn, *resource.Resource) error{
		initTracing(cfg.OTel.Trace),
		initMetering(cfg.OTel.Metric),
		initLogging(cfg.OTel.Log),
	}

	for _, f := range fs {
		err := f(ctx, conn, r)
		if err != nil {
			return err
		}
	}
	return nil
}

func initTracing(otc config.Trace) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
	return func(ctx context.Context, cc *grpc.ClientConn, r *resource.Resource) error {
		if !otc.Enabled || cc == nil {
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

func initMetering(omc config.Metric) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
	return func(ctx context.Context, cc *grpc.ClientConn, r *resource.Resource) error {
		if !omc.Enabled || cc == nil {
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

func initLogging(olc config.Log) func(context.Context, *grpc.ClientConn, *resource.Resource) error {
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

func initLogProcessor(ctx context.Context, olc config.Log, cc *grpc.ClientConn) (log.Processor, error) {
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
