// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"testing"

	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/assert"
	bedrockcfg "github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"google.golang.org/grpc"
)

func TestConfig_InitializeOTel(t *testing.T) {
	t.Run("will not return an error", func(t *testing.T) {
		t.Run("with the default parameters", func(t *testing.T) {
			m, err := bedrockcfg.Read(DefaultConfig())
			if !assert.Nil(t, err) {
				return
			}

			var cfg Config
			err = m.Unmarshal(&cfg)
			if !assert.Nil(t, err) {
				return
			}

			err = cfg.InitializeOTel(context.Background())
			if !assert.Nil(t, err) {
				return
			}
		})
	})
}

func TestTraceProviderInitializer(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it is enabled but no grpc.ClientConn has been initialized", func(t *testing.T) {
			tpi := traceProviderInitializer{
				cfg: config.Trace{
					Enabled: true,
				},
				cc: nil,
			}

			err := tpi.Init(context.Background())
			if !assert.ErrorIs(t, err, ErrOTLPMustBeEnabled) {
				return
			}
		})

		t.Run("if it fails to create the exporter", func(t *testing.T) {
			newExporterErr := errors.New("failed to create exporter")
			tpi := traceProviderInitializer{
				cfg: config.Trace{
					Enabled: true,
				},
				cc: &grpc.ClientConn{},
				newExporter: func(ctx context.Context, o ...otlptracegrpc.Option) (*otlptrace.Exporter, error) {
					return nil, newExporterErr
				},
			}

			err := tpi.Init(context.Background())
			if !assert.ErrorIs(t, err, newExporterErr) {
				return
			}
		})
	})
}

func TestMeterProviderInitializer(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it is enabled but no grpc.ClientConn has been initialized", func(t *testing.T) {
			mpi := meterProviderInitializer{
				cfg: config.Metric{
					Enabled: true,
				},
				cc: nil,
			}

			err := mpi.Init(context.Background())
			if !assert.ErrorIs(t, err, ErrOTLPMustBeEnabled) {
				return
			}
		})

		t.Run("if it fails to create the exporter", func(t *testing.T) {
			newExporterErr := errors.New("failed to create exporter")
			mpi := meterProviderInitializer{
				cfg: config.Metric{
					Enabled: true,
				},
				cc: &grpc.ClientConn{},
				newExporter: func(ctx context.Context, o ...otlpmetricgrpc.Option) (*otlpmetricgrpc.Exporter, error) {
					return nil, newExporterErr
				},
			}

			err := mpi.Init(context.Background())
			if !assert.ErrorIs(t, err, newExporterErr) {
				return
			}
		})
	})
}

func TestLogProviderInitializer(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it is enabled but no grpc.ClientConn has been initialized", func(t *testing.T) {
			lpi := logProviderInitializer{
				cfg: config.Log{
					Enabled: true,
				},
				cc: nil,
			}

			err := lpi.Init(context.Background())
			if !assert.ErrorIs(t, err, ErrOTLPMustBeEnabled) {
				return
			}
		})

		t.Run("if it fails to create the exporter", func(t *testing.T) {
			newExporterErr := errors.New("failed to create exporter")
			lpi := logProviderInitializer{
				cfg: config.Log{
					Enabled: true,
				},
				cc: &grpc.ClientConn{},
				newExporter: func(ctx context.Context, o ...otlploggrpc.Option) (*otlploggrpc.Exporter, error) {
					return nil, newExporterErr
				},
			}

			err := lpi.Init(context.Background())
			if !assert.ErrorIs(t, err, newExporterErr) {
				return
			}
		})
	})
}
