// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/z5labs/humus/queue"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestAtMostOnce(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		withContext  func(context.Context) (context.Context, context.CancelCauseFunc)
		consumer     func(t *testing.T, cancel context.CancelCauseFunc) queue.ConsumerFunc[fetch]
		processor    func(t *testing.T, cancel context.CancelCauseFunc) queue.ProcessorFunc[Message]
		acknowledger func(t *testing.T, cancel context.CancelCauseFunc) queue.AcknowledgerFunc[[]*kgo.Record]
	}{
		{
			name: "should shutdown if context is cancelled while consuming",
			withContext: func(ctx context.Context) (context.Context, context.CancelCauseFunc) {
				return context.WithCancelCause(ctx)
			},
			consumer: func(t *testing.T, cancel context.CancelCauseFunc) queue.ConsumerFunc[fetch] {
				return func(ctx context.Context) (fetch, error) {
					cancel(context.Canceled)
					return fetch{}, nil
				}
			},
			processor: func(t *testing.T, cancel context.CancelCauseFunc) queue.ProcessorFunc[Message] {
				return func(ctx context.Context, msg Message) error {
					require.Fail(t, "should not be called")
					return nil
				}
			},
			acknowledger: func(t *testing.T, cancel context.CancelCauseFunc) queue.AcknowledgerFunc[[]*kgo.Record] {
				return func(ctx context.Context, records []*kgo.Record) error {
					require.Fail(t, "should not be called")
					return nil
				}
			},
		},
		{
			name: "should not fail if processor returns an error",
			withContext: func(ctx context.Context) (context.Context, context.CancelCauseFunc) {
				return context.WithCancelCause(ctx)
			},
			consumer: func(t *testing.T, cancel context.CancelCauseFunc) queue.ConsumerFunc[fetch] {
				return func(ctx context.Context) (fetch, error) {
					f := fetch{
						topicPartition: topicPartition{
							topic:     "test-topic",
							partition: 0,
						},
						records: []*kgo.Record{
							{},
						},
					}
					return f, nil
				}
			},
			processor: func(t *testing.T, cancel context.CancelCauseFunc) queue.ProcessorFunc[Message] {
				var count atomic.Uint64
				return func(ctx context.Context, msg Message) error {
					count.Add(1)
					if count.Load() > 1000 {
						cancel(context.Canceled)
						return nil
					}
					return errors.New("processor failed")
				}
			},
			acknowledger: func(t *testing.T, cancel context.CancelCauseFunc) queue.AcknowledgerFunc[[]*kgo.Record] {
				return func(ctx context.Context, records []*kgo.Record) error {
					return nil
				}
			},
		},
		{
			name: "should not fail if acknowledger returns an error",
			withContext: func(ctx context.Context) (context.Context, context.CancelCauseFunc) {
				return context.WithCancelCause(ctx)
			},
			consumer: func(t *testing.T, cancel context.CancelCauseFunc) queue.ConsumerFunc[fetch] {
				return func(ctx context.Context) (fetch, error) {
					f := fetch{
						topicPartition: topicPartition{
							topic:     "test-topic",
							partition: 0,
						},
						records: []*kgo.Record{
							{},
						},
					}
					return f, nil
				}
			},
			processor: func(t *testing.T, cancel context.CancelCauseFunc) queue.ProcessorFunc[Message] {
				return func(ctx context.Context, msg Message) error {
					return nil
				}
			},
			acknowledger: func(t *testing.T, cancel context.CancelCauseFunc) queue.AcknowledgerFunc[[]*kgo.Record] {
				var count atomic.Uint64
				return func(ctx context.Context, records []*kgo.Record) error {
					count.Add(1)
					if count.Load() > 1000 {
						cancel(context.Canceled)
						return nil
					}
					return errors.New("acknowledger failed")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.withContext(t.Context())
			defer cancel(context.Canceled)

			consumer := tc.consumer(t, cancel)
			processor := tc.processor(t, cancel)
			acknowledger := tc.acknowledger(t, cancel)

			orchestrator := newAtMostOnceOrchestrator(tc.name, processor)

			rt := orchestrator.Orchestrate(consumer, acknowledger)
			err := rt.ProcessQueue(ctx)
			require.Nil(t, err)
		})
	}
}
