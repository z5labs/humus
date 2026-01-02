// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"testing"
	"time"

	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestNewResource(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		opts []ResourceOption
	}{
		{
			name: "uses environment variables",
			env: map[string]string{
				"OTEL_SERVICE_NAME":    "test-service",
				"OTEL_SERVICE_VERSION": "1.0.0",
			},
			opts: []ResourceOption{
				ServiceName(ServiceNameFromEnv()),
				ServiceVersion(ServiceVersionFromEnv()),
			},
		},
		{
			name: "applies overrides",
			opts: []ResourceOption{
				ServiceName(config.ReaderOf("custom-service")),
			},
		},
		{
			name: "handles missing environment variables",
			env:  map[string]string{},
			opts: []ResourceOption{
				ServiceName(ServiceNameFromEnv()),
				ServiceVersion(ServiceVersionFromEnv()),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			res := NewResource(tc.opts...)
			require.NotNil(t, res.SchemaURL)
			require.NotNil(t, res.ServiceName)
			require.NotNil(t, res.ServiceVersion)
		})
	}
}

func TestResource_Read(t *testing.T) {
	testCases := []struct {
		name        string
		resource    Resource
		expectError bool
	}{
		{
			name: "successful resource creation",
			resource: Resource{
				SchemaURL:      config.ReaderOf(""),
				ServiceName:    config.ReaderOf("test-service"),
				ServiceVersion: config.ReaderOf("1.0.0"),
			},
			expectError: false,
		},
		{
			name: "uses defaults when readers return zero values",
			resource: Resource{
				SchemaURL:      config.ReaderOf(""),
				ServiceName:    config.ReaderOf(""),
				ServiceVersion: config.ReaderOf(""),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.resource.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			rsc, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, rsc)
		})
	}
}

func TestNewTraceIDRatioBasedSampler(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		opts []TraceIDRatioBasedSamplerOption
	}{
		{
			name: "uses OTEL_TRACES_SAMPLER_RATIO",
			env: map[string]string{
				"OTEL_TRACES_SAMPLER_RATIO": "0.5",
			},
			opts: []TraceIDRatioBasedSamplerOption{
				TraceIDSampleRatio(TraceIDSampleRatioFromEnv()),
			},
		},
		{
			name: "applies overrides",
			opts: []TraceIDRatioBasedSamplerOption{
				TraceIDSampleRatio(config.ReaderOf(0.75)),
			},
		},
		{
			name: "handles missing environment variable",
			env:  map[string]string{},
			opts: []TraceIDRatioBasedSamplerOption{
				TraceIDSampleRatio(TraceIDSampleRatioFromEnv()),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			sampler := NewTraceIDRatioBasedSampler(tc.opts...)
			require.NotNil(t, sampler.Ratio)
		})
	}
}

func TestTraceIDRatioBasedSampler_Read(t *testing.T) {
	testCases := []struct {
		name     string
		sampler  TraceIDRatioBasedSampler
		expected float64
	}{
		{
			name: "uses configured ratio",
			sampler: TraceIDRatioBasedSampler{
				Ratio: config.ReaderOf(0.5),
			},
			expected: 0.5,
		},
		{
			name: "defaults to 1.0",
			sampler: TraceIDRatioBasedSampler{
				Ratio: config.ReaderFunc[float64](func(ctx context.Context) (config.Value[float64], error) {
					return config.Value[float64]{}, nil
				}),
			},
			expected: 1.0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.sampler.Read(ctx)

			require.NoError(t, err)
			sampler, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, sampler)
		})
	}
}

func TestNewBatchSpanProcessor(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		opts []BatchSpanProcessorOption
	}{
		{
			name: "uses environment variables",
			env: map[string]string{
				"OTEL_BSP_EXPORT_INTERVAL":       "10s",
				"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "1024",
			},
			opts: []BatchSpanProcessorOption{
				ExportInterval(ExportIntervalFromEnv()),
				MaxExportBatchSize(MaxExportBatchSizeFromEnv()),
			},
		},
		{
			name: "applies overrides",
			opts: []BatchSpanProcessorOption{
				ExportInterval(config.ReaderOf(15 * time.Second)),
			},
		},
		{
			name: "handles missing environment variables",
			env:  map[string]string{},
			opts: []BatchSpanProcessorOption{
				ExportInterval(ExportIntervalFromEnv()),
				MaxExportBatchSize(MaxExportBatchSizeFromEnv()),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exporter := config.ReaderFunc[sdktrace.SpanExporter](func(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
				return config.ValueOf[sdktrace.SpanExporter](nil), nil
			})

			bsp := NewBatchSpanProcessor(exporter, tc.opts...)
			require.NotNil(t, bsp.Exporter)
			require.NotNil(t, bsp.ExportInterval)
			require.NotNil(t, bsp.MaxExportBatchSize)
		})
	}
}

func TestBatchSpanProcessor_Read(t *testing.T) {
	testCases := []struct {
		name        string
		processor   BatchSpanProcessor
		expectError bool
	}{
		{
			name: "successful processor creation",
			processor: BatchSpanProcessor{
				Exporter: config.ReaderFunc[sdktrace.SpanExporter](func(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
					return config.ValueOf[sdktrace.SpanExporter](nil), nil
				}),
				ExportInterval:     config.ReaderOf(5 * time.Second),
				MaxExportBatchSize: config.ReaderOf(512),
			},
			expectError: false,
		},
		{
			name: "uses defaults when readers return zero values",
			processor: BatchSpanProcessor{
				Exporter: config.ReaderFunc[sdktrace.SpanExporter](func(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
					return config.ValueOf[sdktrace.SpanExporter](nil), nil
				}),
				ExportInterval: config.ReaderFunc[time.Duration](func(ctx context.Context) (config.Value[time.Duration], error) {
					return config.Value[time.Duration]{}, nil
				}),
				MaxExportBatchSize: config.ReaderFunc[int](func(ctx context.Context) (config.Value[int], error) {
					return config.Value[int]{}, nil
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.processor.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			processor, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, processor)

			defer processor.Shutdown(context.Background())
		})
	}
}

func TestSdkTracerProvider_Read(t *testing.T) {
	testCases := []struct {
		name        string
		provider    SdkTracerProvider
		expectError bool
	}{
		{
			name: "successful tracer provider creation",
			provider: SdkTracerProvider{
				Resource: config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
					rsc, err := resource.New(context.Background(), resource.WithTelemetrySDK())
					if err != nil {
						return config.Value[*resource.Resource]{}, err
					}
					return config.ValueOf(rsc), nil
				}),
				Sampler: config.ReaderFunc[sdktrace.Sampler](func(ctx context.Context) (config.Value[sdktrace.Sampler], error) {
					return config.ValueOf(sdktrace.AlwaysSample()), nil
				}),
				SpanProcessor: config.ReaderFunc[sdktrace.SpanProcessor](func(ctx context.Context) (config.Value[sdktrace.SpanProcessor], error) {
					exp := &mockSpanExporter{}
					return config.ValueOf[sdktrace.SpanProcessor](sdktrace.NewBatchSpanProcessor(exp)), nil
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.provider.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			tp, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, tp)

			if sdkTp, ok := tp.(*sdktrace.TracerProvider); ok {
				defer sdkTp.Shutdown(context.Background())
			}
		})
	}
}

type mockSpanExporter struct{}

func (m *mockSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return nil
}

func (m *mockSpanExporter) Shutdown(ctx context.Context) error {
	return nil
}

func TestNewPeriodicReader(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		opts []PeriodicReaderOption
	}{
		{
			name: "uses OTEL_METRIC_EXPORT_INTERVAL",
			env: map[string]string{
				"OTEL_METRIC_EXPORT_INTERVAL": "30s",
			},
			opts: []PeriodicReaderOption{
				ExportIntervalMetric(ExportIntervalMetricFromEnv()),
			},
		},
		{
			name: "applies overrides",
			opts: []PeriodicReaderOption{
				ExportIntervalMetric(config.ReaderOf(60 * time.Second)),
			},
		},
		{
			name: "handles missing environment variable",
			env:  map[string]string{},
			opts: []PeriodicReaderOption{
				ExportIntervalMetric(ExportIntervalMetricFromEnv()),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exporter := config.ReaderFunc[sdkmetric.Exporter](func(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
				return config.ValueOf[sdkmetric.Exporter](nil), nil
			})

			pr := NewPeriodicReader(exporter, tc.opts...)
			require.NotNil(t, pr.Exporter)
			require.NotNil(t, pr.ExportInterval)
		})
	}
}

func TestPeriodicReader_Read(t *testing.T) {
	testCases := []struct {
		name        string
		reader      PeriodicReader
		expectError bool
	}{
		{
			name: "successful reader creation",
			reader: PeriodicReader{
				Exporter: config.ReaderFunc[sdkmetric.Exporter](func(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
					return config.ValueOf[sdkmetric.Exporter](&mockMetricExporter{}), nil
				}),
				ExportInterval: config.ReaderOf(30 * time.Second),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.reader.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			reader, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, reader)

			defer reader.Shutdown(context.Background())
		})
	}
}

type mockMetricExporter struct{}

func (m *mockMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	return nil
}

func (m *mockMetricExporter) ForceFlush(ctx context.Context) error {
	return nil
}

func (m *mockMetricExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (m *mockMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

func (m *mockMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return nil
}

func TestSdkMeterProvider_Read(t *testing.T) {
	testCases := []struct {
		name        string
		provider    SdkMeterProvider
		expectError bool
	}{
		{
			name: "successful meter provider creation",
			provider: SdkMeterProvider{
				Resource: config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
					rsc, err := resource.New(context.Background(), resource.WithTelemetrySDK())
					if err != nil {
						return config.Value[*resource.Resource]{}, err
					}
					return config.ValueOf(rsc), nil
				}),
				Reader: config.ReaderFunc[sdkmetric.Reader](func(ctx context.Context) (config.Value[sdkmetric.Reader], error) {
					return config.ValueOf[sdkmetric.Reader](sdkmetric.NewPeriodicReader(&mockMetricExporter{}, sdkmetric.WithInterval(30*time.Second))), nil
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.provider.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			mp, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, mp)

			if sdkMp, ok := mp.(*sdkmetric.MeterProvider); ok {
				defer sdkMp.Shutdown(context.Background())
			}
		})
	}
}

func TestNewBatchLogProcessor(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		opts []BatchLogProcessorOption
	}{
		{
			name: "uses environment variables",
			env: map[string]string{
				"OTEL_BLP_EXPORT_INTERVAL":       "5s",
				"OTEL_BLP_MAX_EXPORT_BATCH_SIZE": "256",
			},
			opts: []BatchLogProcessorOption{
				ExportIntervalLog(ExportIntervalLogFromEnv()),
				MaxExportBatchSizeLog(MaxExportBatchSizeLogFromEnv()),
			},
		},
		{
			name: "applies overrides",
			opts: []BatchLogProcessorOption{
				ExportIntervalLog(config.ReaderOf(10 * time.Second)),
			},
		},
		{
			name: "handles missing environment variables",
			env:  map[string]string{},
			opts: []BatchLogProcessorOption{
				ExportIntervalLog(ExportIntervalLogFromEnv()),
				MaxExportBatchSizeLog(MaxExportBatchSizeLogFromEnv()),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exporter := config.ReaderFunc[sdklog.Exporter](func(ctx context.Context) (config.Value[sdklog.Exporter], error) {
				return config.ValueOf[sdklog.Exporter](nil), nil
			})

			blp := NewBatchLogProcessor(exporter, tc.opts...)
			require.NotNil(t, blp.Exporter)
			require.NotNil(t, blp.ExportInterval)
			require.NotNil(t, blp.MaxExportBatchSize)
		})
	}
}

func TestBatchLogProcessor_Read(t *testing.T) {
	testCases := []struct {
		name        string
		processor   BatchLogProcessor
		expectError bool
	}{
		{
			name: "successful processor creation",
			processor: BatchLogProcessor{
				Exporter: config.ReaderFunc[sdklog.Exporter](func(ctx context.Context) (config.Value[sdklog.Exporter], error) {
					return config.ValueOf[sdklog.Exporter](&mockLogExporter{}), nil
				}),
				ExportInterval:     config.ReaderOf(5 * time.Second),
				MaxExportBatchSize: config.ReaderOf(256),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.processor.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			processor, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, processor)

			defer processor.Shutdown(context.Background())
		})
	}
}

type mockLogExporter struct{}

func (m *mockLogExporter) Export(ctx context.Context, logs []sdklog.Record) error {
	return nil
}

func (m *mockLogExporter) ForceFlush(ctx context.Context) error {
	return nil
}

func (m *mockLogExporter) Shutdown(ctx context.Context) error {
	return nil
}

func TestSdkLoggerProvider_Read(t *testing.T) {
	testCases := []struct {
		name        string
		provider    SdkLoggerProvider
		expectError bool
	}{
		{
			name: "successful logger provider creation",
			provider: SdkLoggerProvider{
				Resource: config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
					rsc, err := resource.New(context.Background(), resource.WithTelemetrySDK())
					if err != nil {
						return config.Value[*resource.Resource]{}, err
					}
					return config.ValueOf(rsc), nil
				}),
				LogProcessor: config.ReaderFunc[sdklog.Processor](func(ctx context.Context) (config.Value[sdklog.Processor], error) {
					return config.ValueOf[sdklog.Processor](sdklog.NewBatchProcessor(&mockLogExporter{})), nil
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			val, err := tc.provider.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			lp, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, lp)

			if sdkLp, ok := lp.(*sdklog.LoggerProvider); ok {
				defer sdkLp.Shutdown(context.Background())
			}
		})
	}
}
