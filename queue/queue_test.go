// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/z5labs/humus/app"

	"github.com/stretchr/testify/require"
)

type captureHandler struct {
	slog.Handler
	records []slog.Record
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.records = append(h.records, record)
	return nil
}

type queueRuntimeFunc func(context.Context) error

func (f queueRuntimeFunc) ProcessQueue(ctx context.Context) error {
	return f(ctx)
}

type failingBuilder struct{}

func (fb failingBuilder) Build(ctx context.Context) (Runtime, error) {
	return Runtime{}, errors.New("build failed")
}

func TestBuild(t *testing.T) {
	t.Run("returns a valid builder", func(t *testing.T) {
		queueRuntime := queueRuntimeFunc(func(ctx context.Context) error {
			return nil
		})

		builder := Build(queueRuntime)
		require.NotNil(t, builder)
	})
}

func TestRun(t *testing.T) {
	t.Run("logs errors from builder", func(t *testing.T) {
		logHandler := &captureHandler{
			Handler: slog.Default().Handler(),
		}

		builder := failingBuilder{}
		err := Run(context.Background(), builder, LogHandler(logHandler))

		require.Error(t, err)
		require.Len(t, logHandler.records, 1)

		record := logHandler.records[0]
		var caughtErr error
		record.Attrs(func(a slog.Attr) bool {
			if a.Key != "error" {
				return true
			}

			v := a.Value.Any()
			err, ok := v.(error)
			if !ok {
				caughtErr = fmt.Errorf("expected attr to be error: %v", a.Value)
				return false
			}
			caughtErr = err
			return false
		})
		require.Error(t, caughtErr)
		require.Equal(t, "build failed", caughtErr.Error())
	})

	t.Run("logs errors from runtime", func(t *testing.T) {
		runtimeErr := errors.New("failed to process queue")
		queueRuntime := queueRuntimeFunc(func(ctx context.Context) error {
			return runtimeErr
		})

		builder := Build(queueRuntime)

		logHandler := &captureHandler{
			Handler: slog.Default().Handler(),
		}

		err := Run(context.Background(), builder, LogHandler(logHandler))

		require.ErrorIs(t, err, runtimeErr)
		require.Len(t, logHandler.records, 1)

		record := logHandler.records[0]
		var caughtErr error
		record.Attrs(func(a slog.Attr) bool {
			if a.Key != "error" {
				return true
			}

			v := a.Value.Any()
			err, ok := v.(error)
			if !ok {
				caughtErr = fmt.Errorf("expected attr to be error: %v", a.Value)
				return false
			}
			caughtErr = err
			return false
		})
		require.ErrorIs(t, caughtErr, runtimeErr)
	})

	t.Run("uses custom log handler when provided", func(t *testing.T) {
		logHandler := &captureHandler{
			Handler: slog.Default().Handler(),
		}

		builder := app.BuilderFunc[Runtime](func(ctx context.Context) (Runtime, error) {
			return Runtime{}, errors.New("test error")
		})

		Run(context.Background(), builder, LogHandler(logHandler))

		require.Len(t, logHandler.records, 1)
	})
}

// Mock implementations for testing

type callRecorder struct {
	calls []string
	mu    sync.Mutex
}

func (r *callRecorder) record(method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method)
}

func (r *callRecorder) getCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.calls...)
}

type mockConsumer[T any] struct {
	consumeFunc func(context.Context) (T, error)
	callCount   int
	recorder    *callRecorder
}

func (m *mockConsumer[T]) Consume(ctx context.Context) (T, error) {
	m.callCount++
	if m.recorder != nil {
		m.recorder.record("Consume")
	}
	if m.consumeFunc != nil {
		return m.consumeFunc(ctx)
	}
	var zero T
	return zero, nil
}

type mockProcessor[T any] struct {
	processFunc func(context.Context, T) error
	callCount   int
	lastItem    T
	recorder    *callRecorder
}

func (m *mockProcessor[T]) Process(ctx context.Context, item T) error {
	m.callCount++
	m.lastItem = item
	if m.recorder != nil {
		m.recorder.record("Process")
	}
	if m.processFunc != nil {
		return m.processFunc(ctx, item)
	}
	return nil
}

type mockAcknowledger[T any] struct {
	ackFunc   func(context.Context, T) error
	callCount int
	lastItem  T
	recorder  *callRecorder
}

func (m *mockAcknowledger[T]) Acknowledge(ctx context.Context, item T) error {
	m.callCount++
	m.lastItem = item
	if m.recorder != nil {
		m.recorder.record("Acknowledge")
	}
	if m.ackFunc != nil {
		return m.ackFunc(ctx, item)
	}
	return nil
}
