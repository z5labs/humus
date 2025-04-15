// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/internal/detector"

	bedrockcfg "github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConfigSource standardizes the template for configuration of humus applications.
// The [io.Reader] is expected to be YAML with support for Go templating. Currently,
// only 2 template functions are supported:
//   - env - this allows environment variables to be substituted into the YAML
//   - default - define a default value in case the original value is nil
func ConfigSource(r io.Reader) bedrockcfg.Source {
	return bedrockcfg.FromYaml(
		bedrockcfg.RenderTextTemplate(
			r,
			bedrockcfg.TemplateFunc("env", func(key string) any {
				v, ok := os.LookupEnv(key)
				if ok {
					return v
				}
				return nil
			}),
			bedrockcfg.TemplateFunc("default", func(def, v any) any {
				if v == nil {
					return def
				}
				return v
			}),
		),
	)
}

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig returns the default config source which corresponds to the [Config] type.
func DefaultConfig() bedrockcfg.Source {
	return ConfigSource(bytes.NewReader(defaultConfig))
}

// Config defines the common configuration for all humus based applications.
type Config struct {
	OTel config.OTel `config:"otel"`
}

// InitializeOTel implements the [appbuilder.OTelInitializer] interface.
func (cfg Config) InitializeOTel(ctx context.Context) error {
	conn, err := newClientConn(cfg.OTel.OTLP)
	if err != nil {
		return err
	}

	r, err := detectResource(ctx, cfg.OTel.Resource)
	if err != nil {
		return err
	}

	initers := []initializer{
		traceProviderInitializer{
			cfg:         cfg.OTel.Trace,
			cc:          conn,
			r:           r,
			newExporter: otlptracegrpc.New,
		},
		meterProviderInitializer{
			cfg:         cfg.OTel.Metric,
			cc:          conn,
			r:           r,
			newExporter: otlpmetricgrpc.New,
		},
		logProviderInitializer{
			cfg:         cfg.OTel.Log,
			cc:          conn,
			r:           r,
			newExporter: otlploggrpc.New,
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

func newClientConn(cfg config.OTLP) (*grpc.ClientConn, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	return grpc.NewClient(
		cfg.Target,
		// TODO: support secure transport credentials
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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

var ErrOTLPMustBeEnabled = errors.New("missing otlp client conn")

type initializer interface {
	Init(context.Context) error
}

type traceProviderInitializer struct {
	cfg         config.Trace
	cc          *grpc.ClientConn
	r           *resource.Resource
	newExporter func(context.Context, ...otlptracegrpc.Option) (*otlptrace.Exporter, error)
}

func (tpi traceProviderInitializer) Init(ctx context.Context) error {
	if !tpi.cfg.Enabled {
		return nil
	}
	if tpi.cc == nil {
		return fmt.Errorf("can not enable otel tracing: %w", ErrOTLPMustBeEnabled)
	}

	exp, err := tpi.newExporter(ctx, otlptracegrpc.WithGRPCConn(tpi.cc))
	if err != nil {
		return err
	}

	bsp := trace.NewBatchSpanProcessor(
		exp,
		trace.WithBatchTimeout(tpi.cfg.BatchTimeout),
	)

	tp := trace.NewTracerProvider(
		trace.WithSpanProcessor(bsp),
		trace.WithSampler(trace.TraceIDRatioBased(tpi.cfg.Sampling)),
		trace.WithResource(tpi.r),
	)
	otel.SetTracerProvider(tp)
	return nil
}

type meterProviderInitializer struct {
	cfg         config.Metric
	cc          *grpc.ClientConn
	r           *resource.Resource
	newExporter func(context.Context, ...otlpmetricgrpc.Option) (*otlpmetricgrpc.Exporter, error)
}

func (mpi meterProviderInitializer) Init(ctx context.Context) error {
	if !mpi.cfg.Enabled {
		return nil
	}
	if mpi.cc == nil {
		return fmt.Errorf("can not enable otel metering: %w", ErrOTLPMustBeEnabled)
	}

	exp, err := mpi.newExporter(ctx, otlpmetricgrpc.WithGRPCConn(mpi.cc))
	if err != nil {
		return err
	}

	pr := metric.NewPeriodicReader(
		exp,
		metric.WithInterval(mpi.cfg.ExportInterval),
		metric.WithProducer(runtime.NewProducer()),
	)

	mp := metric.NewMeterProvider(
		metric.WithReader(pr),
		metric.WithResource(mpi.r),
	)
	otel.SetMeterProvider(mp)

	return runtime.Start(
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	)
}

type logProviderInitializer struct {
	cfg         config.Log
	cc          *grpc.ClientConn
	r           *resource.Resource
	newExporter func(context.Context, ...otlploggrpc.Option) (*otlploggrpc.Exporter, error)
}

func (lpi logProviderInitializer) Init(ctx context.Context) error {
	p, err := lpi.initLogProcessor(ctx)
	if err != nil {
		return err
	}

	lp := log.NewLoggerProvider(
		log.WithProcessor(p),
		log.WithResource(lpi.r),
	)
	global.SetLoggerProvider(lp)

	log := Logger("otel")
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Error("encoutered error from otel sdk", slog.Any("error", err))
	}))
	return nil
}

func (lpi logProviderInitializer) initLogProcessor(ctx context.Context) (log.Processor, error) {
	// TODO: this needs to be made more specific it should either always be OTLP or STDOUT
	//		 the enabled config is a bit confusing to interpret
	if !lpi.cfg.Enabled {
		exp, err := stdoutlog.New()
		if err != nil {
			return nil, err
		}

		sp := log.NewSimpleProcessor(exp)
		return sp, nil
	}
	if lpi.cc == nil {
		return nil, fmt.Errorf("can not enable otel logging: %w", ErrOTLPMustBeEnabled)
	}

	exp, err := lpi.newExporter(ctx, otlploggrpc.WithGRPCConn(lpi.cc))
	if err != nil {
		return nil, err
	}

	bsp := log.NewBatchProcessor(exp)
	return bsp, nil
}
