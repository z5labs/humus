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
	"strings"
	"sync"
	"testing"

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

type runtimeFunc func(context.Context) error

func (f runtimeFunc) ProcessQueue(ctx context.Context) error {
	return f(ctx)
}

func TestRun(t *testing.T) {
	t.Run("will handle error", func(t *testing.T) {
		t.Run("if it fails to build the App", func(t *testing.T) {
			r := strings.NewReader(``)

			buildErr := errors.New("failed to build app")
			b := func(ctx context.Context, cfg Config) (*App, error) {
				return nil, buildErr
			}

			logHandler := &captureHandler{
				Handler: slog.Default().Handler(),
			}

			Run(r, b, LogHandler(logHandler))

			records := logHandler.records
			require.Len(t, records, 1)

			record := records[0]
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
			require.ErrorIs(t, caughtErr, buildErr)
		})

		t.Run("if the runtime returns an error while running", func(t *testing.T) {
			r := strings.NewReader(``)

			runtimeErr := errors.New("failed to process queue")
			rt := runtimeFunc(func(ctx context.Context) error {
				return runtimeErr
			})

			a := NewApp(rt)

			b := func(ctx context.Context, cfg Config) (*App, error) {
				return a, nil
			}

			logHandler := &captureHandler{
				Handler: slog.Default().Handler(),
			}

			Run(r, b, LogHandler(logHandler))

			records := logHandler.records
			require.Len(t, records, 1)

			record := records[0]
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

func TestProcessAtMostOnce(t *testing.T) {
	t.Run("will process item successfully", func(t *testing.T) {
		t.Run("when all operations succeed", func(t *testing.T) {
			ctx := context.Background()
			recorder := &callRecorder{}

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
				recorder: recorder,
			}

			processor := &mockProcessor[string]{
				recorder: recorder,
			}

			acknowledger := &mockAcknowledger[string]{
				recorder: recorder,
			}

			p := ProcessAtMostOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.NoError(t, err)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 1, processor.callCount)
			require.Equal(t, 1, acknowledger.callCount)
			require.Equal(t, "test-message", processor.lastItem)
			require.Equal(t, "test-message", acknowledger.lastItem)

			// Verify call order: Consume → Acknowledge → Process
			calls := recorder.getCalls()
			require.Equal(t, []string{"Consume", "Acknowledge", "Process"}, calls)
		})
	})

	t.Run("will handle error", func(t *testing.T) {
		t.Run("if consumer returns an error", func(t *testing.T) {
			ctx := context.Background()
			consumerErr := errors.New("consume failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "", consumerErr
				},
			}

			processor := &mockProcessor[string]{}
			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtMostOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, consumerErr)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 0, processor.callCount)
			require.Equal(t, 0, acknowledger.callCount)
		})

		t.Run("if consumer returns ErrEndOfQueue", func(t *testing.T) {
			ctx := context.Background()

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "", ErrEndOfQueue
				},
			}

			processor := &mockProcessor[string]{}
			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtMostOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, ErrEndOfQueue)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 0, processor.callCount)
			require.Equal(t, 0, acknowledger.callCount)
		})

		t.Run("if acknowledger returns an error", func(t *testing.T) {
			ctx := context.Background()
			ackErr := errors.New("acknowledge failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
			}

			processor := &mockProcessor[string]{}

			acknowledger := &mockAcknowledger[string]{
				ackFunc: func(ctx context.Context, item string) error {
					return ackErr
				},
			}

			p := ProcessAtMostOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, ackErr)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 1, acknowledger.callCount)
			// Processor should NOT be called when acknowledge fails
			require.Equal(t, 0, processor.callCount)
		})

		t.Run("if processor returns an error", func(t *testing.T) {
			ctx := context.Background()
			processErr := errors.New("process failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
			}

			processor := &mockProcessor[string]{
				processFunc: func(ctx context.Context, item string) error {
					return processErr
				},
			}

			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtMostOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, processErr)
			require.Equal(t, 1, consumer.callCount)
			// Message was already acknowledged (at-most-once semantics)
			require.Equal(t, 1, acknowledger.callCount)
			require.Equal(t, 1, processor.callCount)
		})
	})
}

func TestProcessAtLeastOnce(t *testing.T) {
	t.Run("will process item successfully", func(t *testing.T) {
		t.Run("when all operations succeed", func(t *testing.T) {
			ctx := context.Background()
			recorder := &callRecorder{}

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
				recorder: recorder,
			}

			processor := &mockProcessor[string]{
				recorder: recorder,
			}

			acknowledger := &mockAcknowledger[string]{
				recorder: recorder,
			}

			p := ProcessAtLeastOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.NoError(t, err)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 1, processor.callCount)
			require.Equal(t, 1, acknowledger.callCount)
			require.Equal(t, "test-message", processor.lastItem)
			require.Equal(t, "test-message", acknowledger.lastItem)

			// Verify call order: Consume → Process → Acknowledge
			calls := recorder.getCalls()
			require.Equal(t, []string{"Consume", "Process", "Acknowledge"}, calls)
		})
	})

	t.Run("will handle error", func(t *testing.T) {
		t.Run("if consumer returns an error", func(t *testing.T) {
			ctx := context.Background()
			consumerErr := errors.New("consume failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "", consumerErr
				},
			}

			processor := &mockProcessor[string]{}
			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtLeastOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, consumerErr)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 0, processor.callCount)
			require.Equal(t, 0, acknowledger.callCount)
		})

		t.Run("if consumer returns ErrEndOfQueue", func(t *testing.T) {
			ctx := context.Background()

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "", ErrEndOfQueue
				},
			}

			processor := &mockProcessor[string]{}
			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtLeastOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, ErrEndOfQueue)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 0, processor.callCount)
			require.Equal(t, 0, acknowledger.callCount)
		})

		t.Run("if processor returns an error", func(t *testing.T) {
			ctx := context.Background()
			processErr := errors.New("process failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
			}

			processor := &mockProcessor[string]{
				processFunc: func(ctx context.Context, item string) error {
					return processErr
				},
			}

			acknowledger := &mockAcknowledger[string]{}

			p := ProcessAtLeastOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, processErr)
			require.Equal(t, 1, consumer.callCount)
			require.Equal(t, 1, processor.callCount)
			// Acknowledger should NOT be called when processing fails (at-least-once semantics)
			require.Equal(t, 0, acknowledger.callCount)
		})

		t.Run("if acknowledger returns an error", func(t *testing.T) {
			ctx := context.Background()
			ackErr := errors.New("acknowledge failed")

			consumer := &mockConsumer[string]{
				consumeFunc: func(ctx context.Context) (string, error) {
					return "test-message", nil
				},
			}

			processor := &mockProcessor[string]{}

			acknowledger := &mockAcknowledger[string]{
				ackFunc: func(ctx context.Context, item string) error {
					return ackErr
				},
			}

			p := ProcessAtLeastOnce(consumer, processor, acknowledger)
			err := p.ProcessItem(ctx)

			require.ErrorIs(t, err, ackErr)
			require.Equal(t, 1, consumer.callCount)
			// Processing completed successfully before acknowledgment failed
			require.Equal(t, 1, processor.callCount)
			require.Equal(t, 1, acknowledger.callCount)
		})
	})
}
