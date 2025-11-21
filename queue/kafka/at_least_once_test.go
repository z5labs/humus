// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/z5labs/humus/queue"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
)

// mockConsumer implements queue.Consumer[fetch] for testing.
type mockFetchConsumer struct {
	fetches []fetch
	index   int
	err     error
}

func (m *mockFetchConsumer) Consume(ctx context.Context) (fetch, error) {
	if m.err != nil {
		return fetch{}, m.err
	}
	if m.index >= len(m.fetches) {
		return fetch{}, queue.ErrEndOfQueue
	}
	f := m.fetches[m.index]
	m.index++
	return f, nil
}

// mockProcessor implements queue.Processor[Message] for testing.
type mockMessageProcessor struct {
	messages []Message
	err      error
}

func (m *mockMessageProcessor) Process(ctx context.Context, msg Message) error {
	m.messages = append(m.messages, msg)
	return m.err
}

// mockAcknowledger implements queue.Acknowledger[[]*kgo.Record] for testing.
type mockRecordAcknowledger struct {
	acknowledged [][]*kgo.Record
	err          error
}

func (m *mockRecordAcknowledger) Acknowledge(ctx context.Context, records []*kgo.Record) error {
	m.acknowledged = append(m.acknowledged, records)
	return m.err
}

// callRecorder tracks the order of method calls for verifying semantics.
type callRecorder struct {
	calls []string
}

func (c *callRecorder) record(call string) {
	c.calls = append(c.calls, call)
}

// orderTrackingConsumer wraps mockFetchConsumer to track call order.
type orderTrackingConsumer struct {
	*mockFetchConsumer
	recorder *callRecorder
}

func (o *orderTrackingConsumer) Consume(ctx context.Context) (fetch, error) {
	o.recorder.record("Consume")
	return o.mockFetchConsumer.Consume(ctx)
}

// orderTrackingProcessor wraps mockMessageProcessor to track call order.
type orderTrackingProcessor struct {
	*mockMessageProcessor
	recorder *callRecorder
}

func (o *orderTrackingProcessor) Process(ctx context.Context, msg Message) error {
	o.recorder.record("Process")
	return o.mockMessageProcessor.Process(ctx, msg)
}

// orderTrackingAcknowledger wraps mockRecordAcknowledger to track call order.
type orderTrackingAcknowledger struct {
	*mockRecordAcknowledger
	recorder *callRecorder
}

func (o *orderTrackingAcknowledger) Acknowledge(ctx context.Context, records []*kgo.Record) error {
	o.recorder.record("Acknowledge")
	return o.mockRecordAcknowledger.Acknowledge(ctx, records)
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_SuccessfulProcessing(t *testing.T) {
	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test", Partition: 0, Offset: 101, Value: []byte("msg2")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify
	require.NoError(t, err)
	require.Len(t, processor.messages, 2)
	require.Equal(t, []byte("msg1"), processor.messages[0].Value)
	require.Equal(t, []byte("msg2"), processor.messages[1].Value)
	require.Len(t, acknowledger.acknowledged, 1)
	require.Equal(t, records, acknowledger.acknowledged[0])
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_VerifyCallOrder(t *testing.T) {
	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
	}

	// Setup call recorder
	recorder := &callRecorder{}

	// Setup order-tracking mocks
	consumer := &orderTrackingConsumer{
		mockFetchConsumer: &mockFetchConsumer{
			fetches: []fetch{{records: records}},
		},
		recorder: recorder,
	}
	processor := &orderTrackingProcessor{
		mockMessageProcessor: &mockMessageProcessor{},
		recorder:             recorder,
	}
	acknowledger := &orderTrackingAcknowledger{
		mockRecordAcknowledger: &mockRecordAcknowledger{},
		recorder:               recorder,
	}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify call order: Consume → Process → Acknowledge (then Consume again for ErrEndOfQueue)
	require.NoError(t, err)
	require.Len(t, recorder.calls, 4)
	require.Equal(t, "Consume", recorder.calls[0])
	require.Equal(t, "Process", recorder.calls[1])
	require.Equal(t, "Acknowledge", recorder.calls[2])
	require.Equal(t, "Consume", recorder.calls[3]) // Second consume returns ErrEndOfQueue
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_ConsumeError(t *testing.T) {
	ctx := context.Background()

	expectedErr := errors.New("consume failed")

	// Setup mocks
	consumer := &mockFetchConsumer{
		err: expectedErr,
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify error propagates
	require.ErrorIs(t, err, expectedErr)
	require.Len(t, processor.messages, 0)
	require.Len(t, acknowledger.acknowledged, 0)
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_ProcessingErrorLogged(t *testing.T) {
	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test", Partition: 0, Offset: 101, Value: []byte("msg2")},
	}

	processingErr := errors.New("processing failed")

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{
		err: processingErr, // All processing will fail
	}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify: Processing errors are logged but acknowledge still happens
	require.NoError(t, err)
	require.Len(t, processor.messages, 2)
	require.Len(t, acknowledger.acknowledged, 1) // Still acknowledged despite processing errors
	require.Equal(t, records, acknowledger.acknowledged[0])
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_AcknowledgeError(t *testing.T) {
	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
	}

	ackErr := errors.New("acknowledge failed")

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{
		err: ackErr,
	}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify error propagates
	require.ErrorIs(t, err, ackErr)
	require.Len(t, processor.messages, 1) // Processing happened before acknowledge failed
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_EndOfQueue(t *testing.T) {
	ctx := context.Background()

	// Setup mocks - consumer returns ErrEndOfQueue immediately
	consumer := &mockFetchConsumer{
		fetches: []fetch{}, // Empty, will return ErrEndOfQueue
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify graceful return
	require.NoError(t, err)
	require.Len(t, processor.messages, 0)
	require.Len(t, acknowledger.acknowledged, 0)
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify context cancellation is handled
	// Processing should stop early when context is cancelled
	require.Error(t, err)
}

func TestAtLeastOncePartitionRuntime_ProcessQueue_MultipleBatches(t *testing.T) {
	ctx := context.Background()

	// Create multiple batches
	batch1 := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test", Partition: 0, Offset: 101, Value: []byte("msg2")},
	}
	batch2 := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 102, Value: []byte("msg3")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{
			{records: batch1},
			{records: batch2},
		},
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atLeastOncePartitionRuntime{
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify all batches processed
	require.NoError(t, err)
	require.Len(t, processor.messages, 3)
	require.Len(t, acknowledger.acknowledged, 2)
	require.Equal(t, batch1, acknowledger.acknowledged[0])
	require.Equal(t, batch2, acknowledger.acknowledged[1])
}
