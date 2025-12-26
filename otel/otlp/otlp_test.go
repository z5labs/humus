// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otlp

import (
	"context"
	"testing"

	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestGrpcConn_Read(t *testing.T) {
	testCases := []struct {
		name        string
		target      config.Reader[string]
		expectError bool
	}{
		{
			name:        "valid target",
			target:      config.ReaderOf("localhost:4317"),
			expectError: false,
		},
		{
			name:        "empty target creates connection",
			target:      config.ReaderOf(""),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gc := &GrpcConn{
				Target: tc.target,
			}

			ctx := context.Background()
			val, err := gc.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			conn, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, conn)

			defer conn.Close()
		})
	}
}

func TestGrpcTraceExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*GrpcTraceExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*GrpcTraceExporter){
				func(e *GrpcTraceExporter) {
					e.Conn = &GrpcConn{
						Target: config.ReaderOf("custom:4317"),
					}
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := GrpcTraceExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Conn)
		})
	}
}

func TestGrpcTraceExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		conn        config.Reader[*grpc.ClientConn]
		expectError bool
	}{
		{
			name: "successful export creation",
			conn: config.ReaderFunc[*grpc.ClientConn](func(ctx context.Context) (config.Value[*grpc.ClientConn], error) {
				cc, err := grpc.NewClient("localhost:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return config.Value[*grpc.ClientConn]{}, err
				}
				return config.ValueOf(cc), nil
			}),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := GrpcTraceExporter{
				Conn: tc.conn,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())

			connVal, err := tc.conn.Read(ctx)
			require.NoError(t, err)
			conn, _ := connVal.Value()
			defer conn.Close()
		})
	}
}

func TestGrpcMetricExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*GrpcMetricExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*GrpcMetricExporter){
				func(e *GrpcMetricExporter) {
					e.Conn = &GrpcConn{
						Target: config.ReaderOf("custom:4317"),
					}
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := GrpcMetricExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Conn)
		})
	}
}

func TestGrpcMetricExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		conn        config.Reader[*grpc.ClientConn]
		expectError bool
	}{
		{
			name: "successful export creation",
			conn: config.ReaderFunc[*grpc.ClientConn](func(ctx context.Context) (config.Value[*grpc.ClientConn], error) {
				cc, err := grpc.NewClient("localhost:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return config.Value[*grpc.ClientConn]{}, err
				}
				return config.ValueOf(cc), nil
			}),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := GrpcMetricExporter{
				Conn: tc.conn,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())

			connVal, err := tc.conn.Read(ctx)
			require.NoError(t, err)
			conn, _ := connVal.Value()
			defer conn.Close()
		})
	}
}

func TestGrpcLogExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*GrpcLogExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*GrpcLogExporter){
				func(e *GrpcLogExporter) {
					e.Conn = &GrpcConn{
						Target: config.ReaderOf("custom:4317"),
					}
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := GrpcLogExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Conn)
		})
	}
}

func TestGrpcLogExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		conn        config.Reader[*grpc.ClientConn]
		expectError bool
	}{
		{
			name: "successful export creation",
			conn: config.ReaderFunc[*grpc.ClientConn](func(ctx context.Context) (config.Value[*grpc.ClientConn], error) {
				cc, err := grpc.NewClient("localhost:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return config.Value[*grpc.ClientConn]{}, err
				}
				return config.ValueOf(cc), nil
			}),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := GrpcLogExporter{
				Conn: tc.conn,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())

			connVal, err := tc.conn.Read(ctx)
			require.NoError(t, err)
			conn, _ := connVal.Value()
			defer conn.Close()
		})
	}
}

func TestHttpTraceExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*HttpTraceExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*HttpTraceExporter){
				func(e *HttpTraceExporter) {
					e.Endpoint = config.ReaderOf("custom:4318")
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := HttpTraceExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Endpoint)
		})
	}
}

func TestHttpTraceExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		endpoint    config.Reader[string]
		expectError bool
	}{
		{
			name:        "successful export creation",
			endpoint:    config.ReaderOf("localhost:4318"),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := HttpTraceExporter{
				Endpoint: tc.endpoint,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())
		})
	}
}

func TestHttpMetricExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*HttpMetricExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*HttpMetricExporter){
				func(e *HttpMetricExporter) {
					e.Endpoint = config.ReaderOf("custom:4318")
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := HttpMetricExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Endpoint)
		})
	}
}

func TestHttpMetricExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		endpoint    config.Reader[string]
		expectError bool
	}{
		{
			name:        "successful export creation",
			endpoint:    config.ReaderOf("localhost:4318"),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := HttpMetricExporter{
				Endpoint: tc.endpoint,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())
		})
	}
}

func TestHttpLogExporterFromEnv(t *testing.T) {
	testCases := []struct {
		name      string
		env       map[string]string
		overrides []func(*HttpLogExporter)
	}{
		{
			name: "uses OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "falls back to OTEL_EXPORTER_OTLP_ENDPOINT",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4318",
			},
		},
		{
			name: "applies overrides",
			overrides: []func(*HttpLogExporter){
				func(e *HttpLogExporter) {
					e.Endpoint = config.ReaderOf("custom:4318")
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			exp := HttpLogExporterFromEnv(tc.overrides...)
			require.NotNil(t, exp.Endpoint)
		})
	}
}

func TestHttpLogExporter_Read(t *testing.T) {
	testCases := []struct {
		name        string
		endpoint    config.Reader[string]
		expectError bool
	}{
		{
			name:        "successful export creation",
			endpoint:    config.ReaderOf("localhost:4318"),
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exp := HttpLogExporter{
				Endpoint: tc.endpoint,
			}

			ctx := context.Background()
			val, err := exp.Read(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			exporter, ok := val.Value()
			require.True(t, ok)
			require.NotNil(t, exporter)

			defer exporter.Shutdown(context.Background())
		})
	}
}
