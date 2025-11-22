// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
)

// concurrentSafeProcessor wraps mockMessageProcessor with mutex for concurrent access.
type concurrentSafeProcessor struct {
	mu       sync.Mutex
	messages []Message
	err      error
}

func (p *concurrentSafeProcessor) Process(ctx context.Context, msg Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, msg)
	return p.err
}

func (p *concurrentSafeProcessor) getMessages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Message{}, p.messages...)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_SuccessfulProcessing(t *testing.T) {
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
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify
	require.NoError(t, err)
	messages := processor.getMessages()
	require.Len(t, messages, 2)
	require.Len(t, acknowledger.acknowledged, 1)
	require.Equal(t, records, acknowledger.acknowledged[0])
}

func TestAtMostOncePartitionRuntime_ProcessQueue_VerifyCallOrder(t *testing.T) {
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

	// Create a concurrent-safe order tracking processor
	processor := &concurrentOrderTrackingProcessor{
		processor: &concurrentSafeProcessor{},
		recorder:  recorder,
	}

	acknowledger := &orderTrackingAcknowledger{
		mockRecordAcknowledger: &mockRecordAcknowledger{},
		recorder:               recorder,
	}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify calls were made (order may vary due to concurrent processing)
	// With concurrent batch processing, Process happens asynchronously
	require.NoError(t, err)
	calls := recorder.getCalls()
	require.Len(t, calls, 4)
	
	// Verify first call is always Consume
	require.Equal(t, "Consume", calls[0])
	
	// Verify Acknowledge happens (at-most-once: acknowledge before process)
	require.Contains(t, calls, "Acknowledge")
	
	// Verify Process happens
	require.Contains(t, calls, "Process")
	
	// Count the occurrences of each call type
	consumeCount := 0
	for _, call := range calls {
		if call == "Consume" {
			consumeCount++
		}
	}
	// Second consume returns ErrEndOfQueue
	require.Equal(t, 2, consumeCount)
}

// concurrentOrderTrackingProcessor tracks call order for concurrent processing.
type concurrentOrderTrackingProcessor struct {
	processor *concurrentSafeProcessor
	recorder  *callRecorder
	mu        sync.Mutex
}

func (p *concurrentOrderTrackingProcessor) Process(ctx context.Context, msg Message) error {
	p.mu.Lock()
	p.recorder.record("Process")
	p.mu.Unlock()
	return p.processor.Process(ctx, msg)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_AcknowledgeBeforeProcess(t *testing.T) {
	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
	}

	// Setup call tracking
	var callOrder []string
	var mu sync.Mutex

	// Create consumer
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}

	// Create processor that records when it's called
	processor := &processorFunc{
		fn: func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			callOrder = append(callOrder, "process")
			return nil
		},
	}

	// Create acknowledger that records when it's called
	acknowledger := &acknowledgerFunc{
		fn: func(ctx context.Context, recs []*kgo.Record) error {
			mu.Lock()
			defer mu.Unlock()
			callOrder = append(callOrder, "acknowledge")
			return nil
		},
	}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify acknowledge happens before process
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, callOrder, 2)
	require.Equal(t, "acknowledge", callOrder[0])
	require.Equal(t, "process", callOrder[1])
}

// processorFunc implements queue.Processor using a function.
type processorFunc struct {
	fn func(context.Context, Message) error
}

func (f *processorFunc) Process(ctx context.Context, msg Message) error {
	return f.fn(ctx, msg)
}

// acknowledgerFunc implements queue.Acknowledger using a function.
type acknowledgerFunc struct {
	fn func(context.Context, []*kgo.Record) error
}

func (f *acknowledgerFunc) Acknowledge(ctx context.Context, records []*kgo.Record) error {
	return f.fn(ctx, records)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_ConsumeError(t *testing.T) {
	ctx := context.Background()

	expectedErr := errors.New("consume failed")

	// Setup mocks
	consumer := &mockFetchConsumer{
		err: expectedErr,
	}
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify error propagates
	require.ErrorIs(t, err, expectedErr)
	require.Len(t, processor.getMessages(), 0)
	require.Len(t, acknowledger.acknowledged, 0)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_ProcessingErrorDoesNotPropagate(t *testing.T) {
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
	processor := &concurrentSafeProcessor{
		err: processingErr, // All processing will fail
	}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify: Processing errors do NOT propagate (messages already committed)
	require.NoError(t, err)
	messages := processor.getMessages()
	require.Len(t, messages, 2)
	require.Len(t, acknowledger.acknowledged, 1) // Already acknowledged before processing
}

func TestAtMostOncePartitionRuntime_ProcessQueue_AcknowledgeError(t *testing.T) {
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
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{
		err: ackErr,
	}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify error propagates
	require.ErrorIs(t, err, ackErr)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_EndOfQueue(t *testing.T) {
	ctx := context.Background()

	// Setup mocks - consumer returns ErrEndOfQueue immediately
	consumer := &mockFetchConsumer{
		fetches: []fetch{}, // Empty, will return ErrEndOfQueue
	}
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify graceful return
	require.NoError(t, err)
	require.Len(t, processor.getMessages(), 0)
	require.Len(t, acknowledger.acknowledged, 0)
}

func TestAtMostOncePartitionRuntime_ProcessQueue_ConcurrentProcessing(t *testing.T) {
	ctx := context.Background()

	// Create multiple records to ensure concurrent processing
	records := []*kgo.Record{
		{Topic: "test", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test", Partition: 0, Offset: 101, Value: []byte("msg2")},
		{Topic: "test", Partition: 0, Offset: 102, Value: []byte("msg3")},
		{Topic: "test", Partition: 0, Offset: 103, Value: []byte("msg4")},
		{Topic: "test", Partition: 0, Offset: 104, Value: []byte("msg5")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify all messages processed (order may vary due to concurrency)
	require.NoError(t, err)
	messages := processor.getMessages()
	require.Len(t, messages, 5)

	// Verify all unique message values are present
	valueSet := make(map[string]bool)
	for _, msg := range messages {
		valueSet[string(msg.Value)] = true
	}
	require.Len(t, valueSet, 5)
	require.True(t, valueSet["msg1"])
	require.True(t, valueSet["msg2"])
	require.True(t, valueSet["msg3"])
	require.True(t, valueSet["msg4"])
	require.True(t, valueSet["msg5"])
}

func TestAtMostOncePartitionRuntime_ProcessQueue_MultipleBatches(t *testing.T) {
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
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create runtime
	runtime := &atMostOncePartitionRuntime{
		log:          slog.Default(),
		tracer:       otel.Tracer("test"),
		consumer:     consumer,
		processor:    processor,
		acknowledger: acknowledger,
	}

	// Execute
	err := runtime.ProcessQueue(ctx)

	// Verify all batches processed
	require.NoError(t, err)
	messages := processor.getMessages()
	require.Len(t, messages, 3)
	require.Len(t, acknowledger.acknowledged, 2)
	require.Equal(t, batch1, acknowledger.acknowledged[0])
	require.Equal(t, batch2, acknowledger.acknowledged[1])
}
