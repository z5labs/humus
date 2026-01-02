// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type mockRuntime struct {
	runCalled bool
	runErr    error
}

func (m *mockRuntime) Run(ctx context.Context) error {
	m.runCalled = true
	return m.runErr
}

type mockShutdowner struct {
	shutdownCalled bool
	shutdownErr    error
}

func (m *mockShutdowner) Shutdown(ctx context.Context) error {
	m.shutdownCalled = true
	return m.shutdownErr
}

func TestBuild(t *testing.T) {
	testCases := []struct {
		name        string
		sdk         SDK
		expectError bool
	}{
		{
			name: "builds runtime with all providers",
			sdk: SDK{
				TextMapPropagator: config.ReaderOf[propagation.TextMapPropagator](propagation.TraceContext{}),
				TracerProvider:    config.ReaderOf[trace.TracerProvider](tracenoop.NewTracerProvider()),
				MeterProvider:     config.ReaderOf[metric.MeterProvider](metricnoop.NewMeterProvider()),
				LoggerProvider:    config.ReaderOf[log.LoggerProvider](lognoop.NewLoggerProvider()),
			},
			expectError: false,
		},
		{
			name: "builds runtime with nil providers using defaults",
			sdk: SDK{
				TextMapPropagator: config.ReaderFunc[propagation.TextMapPropagator](func(ctx context.Context) (config.Value[propagation.TextMapPropagator], error) {
					return config.Value[propagation.TextMapPropagator]{}, nil
				}),
				TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
					return config.Value[trace.TracerProvider]{}, nil
				}),
				MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
					return config.Value[metric.MeterProvider]{}, nil
				}),
				LoggerProvider: config.ReaderFunc[log.LoggerProvider](func(ctx context.Context) (config.Value[log.LoggerProvider], error) {
					return config.Value[log.LoggerProvider]{}, nil
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockRuntime{}
			rtBuilder := app.BuilderFunc[*mockRuntime](func(ctx context.Context) (*mockRuntime, error) {
				return mock, nil
			})

			builder := Build(tc.sdk, rtBuilder)
			require.NotNil(t, builder)

			ctx := context.Background()
			rt, err := builder.Build(ctx)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, rt)
			require.NotNil(t, rt.textMapPropagator)
			require.NotNil(t, rt.tracerProvider)
			require.NotNil(t, rt.meterProvider)
			require.NotNil(t, rt.loggerProvider)
		})
	}
}

func TestRuntime_Run(t *testing.T) {
	testCases := []struct {
		name        string
		innerErr    error
		expectError bool
	}{
		{
			name:        "successful run",
			innerErr:    nil,
			expectError: false,
		},
		{
			name:        "propagates inner runtime error",
			innerErr:    errors.New("runtime error"),
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockRuntime{runErr: tc.innerErr}

			rt := Runtime{
				inner:             mock,
				textMapPropagator: propagation.TraceContext{},
				tracerProvider:    tracenoop.NewTracerProvider(),
				meterProvider:     metricnoop.NewMeterProvider(),
				loggerProvider:    lognoop.NewLoggerProvider(),
			}

			ctx := context.Background()
			err := rt.Run(ctx)

			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.True(t, mock.runCalled)
		})
	}
}

func TestShutdown(t *testing.T) {
	testCases := []struct {
		name        string
		shutdowners []any
		expectError bool
	}{
		{
			name: "shuts down all providers",
			shutdowners: []any{
				&mockShutdowner{},
				&mockShutdowner{},
			},
			expectError: false,
		},
		{
			name: "handles non-shutdowner values",
			shutdowners: []any{
				"not a shutdowner",
				42,
			},
			expectError: false,
		},
		{
			name: "collects all shutdown errors",
			shutdowners: []any{
				&mockShutdowner{shutdownErr: errors.New("error 1")},
				&mockShutdowner{shutdownErr: errors.New("error 2")},
			},
			expectError: true,
		},
		{
			name: "continues shutdown on error",
			shutdowners: []any{
				&mockShutdowner{shutdownErr: errors.New("error 1")},
				&mockShutdowner{},
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fn := shutdown(tc.shutdowners...)
			err := fn()

			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			for _, v := range tc.shutdowners {
				if m, ok := v.(*mockShutdowner); ok {
					require.True(t, m.shutdownCalled)
				}
			}
		})
	}
}

func TestCloserFunc(t *testing.T) {
	called := false
	fn := closerFunc(func() error {
		called = true
		return nil
	})

	err := fn.Close()
	require.NoError(t, err)
	require.True(t, called)
}

func TestCloserFunc_withError(t *testing.T) {
	expectedErr := errors.New("close error")
	fn := closerFunc(func() error {
		return expectedErr
	})

	err := fn.Close()
	require.Error(t, err)
	require.Equal(t, expectedErr, err)
}
