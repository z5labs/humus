// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestAtLeastOncePartitionRuntime_MetricsRecorded(t *testing.T) {
	// Setup metric reader to capture metrics
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = provider.Shutdown(context.Background())
	}()

	// Set global meter provider for this test
	otel.SetMeterProvider(provider)

	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test-topic", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test-topic", Partition: 0, Offset: 101, Value: []byte("msg2")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create orchestrator and runtime
	orchestrator := newAtLeastOnceOrchestrator("test-group", processor)
	runtime := orchestrator.Orchestrate(consumer, acknowledger)

	// Execute
	err := runtime.ProcessQueue(ctx)
	require.NoError(t, err)

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(ctx, &rm)
	require.NoError(t, err)

	// Verify metrics were recorded
	require.Len(t, rm.ScopeMetrics, 1)
	metrics := rm.ScopeMetrics[0].Metrics

	// Find our metrics
	var processedCount, committedCount int64
	for i := range metrics {
		switch metrics[i].Name {
		case "kafka.consumer.messages.processed":
			processedSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range processedSum.DataPoints {
				processedCount += dp.Value
			}
		case "kafka.consumer.messages.committed":
			committedSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range committedSum.DataPoints {
				committedCount += dp.Value
			}
		}
	}

	// Verify both metrics were recorded with correct counts
	require.Equal(t, int64(2), processedCount, "should process 2 messages")
	require.Equal(t, int64(2), committedCount, "should commit 2 messages")
}

func TestAtLeastOncePartitionRuntime_MetricsRecordProcessingFailures(t *testing.T) {
	// Setup metric reader to capture metrics
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = provider.Shutdown(context.Background())
	}()

	// Set global meter provider for this test
	otel.SetMeterProvider(provider)

	ctx := context.Background()

	processingErr := errors.New("processing failed")

	// Create test records
	records := []*kgo.Record{
		{Topic: "test-topic", Partition: 0, Offset: 100, Value: []byte("msg1")},
		{Topic: "test-topic", Partition: 0, Offset: 101, Value: []byte("msg2")},
	}

	// Setup mocks - processor will fail
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &mockMessageProcessor{
		err: processingErr,
	}
	acknowledger := &mockRecordAcknowledger{}

	// Create orchestrator and runtime
	orchestrator := newAtLeastOnceOrchestrator("test-group", processor)
	runtime := orchestrator.Orchestrate(consumer, acknowledger)

	// Execute
	err := runtime.ProcessQueue(ctx)
	require.NoError(t, err) // Processing errors are logged but don't propagate

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(ctx, &rm)
	require.NoError(t, err)

	// Verify metrics were recorded
	require.Len(t, rm.ScopeMetrics, 1)
	metrics := rm.ScopeMetrics[0].Metrics

	// Find failure metric
	var failureCount int64
	for i := range metrics {
		if metrics[i].Name == "kafka.consumer.processing.failures" {
			failureSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range failureSum.DataPoints {
				failureCount += dp.Value
			}
			break
		}
	}

	require.Equal(t, int64(2), failureCount, "should record 2 failures")
}

func TestAtMostOncePartitionRuntime_MetricsRecorded(t *testing.T) {
	// Setup metric reader to capture metrics
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = provider.Shutdown(context.Background())
	}()

	// Set global meter provider for this test
	otel.SetMeterProvider(provider)

	ctx := context.Background()

	// Create test records
	records := []*kgo.Record{
		{Topic: "test-topic", Partition: 1, Offset: 200, Value: []byte("msg1")},
		{Topic: "test-topic", Partition: 1, Offset: 201, Value: []byte("msg2")},
	}

	// Setup mocks
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &concurrentSafeProcessor{}
	acknowledger := &mockRecordAcknowledger{}

	// Create orchestrator and runtime
	orchestrator := newAtMostOnceOrchestrator("test-group", processor)
	runtime := orchestrator.Orchestrate(consumer, acknowledger)

	// Execute
	err := runtime.ProcessQueue(ctx)
	require.NoError(t, err)

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(ctx, &rm)
	require.NoError(t, err)

	// Verify metrics were recorded
	require.Len(t, rm.ScopeMetrics, 1)
	metrics := rm.ScopeMetrics[0].Metrics

	// Find our metrics
	var processedCount, committedCount int64
	for i := range metrics {
		switch metrics[i].Name {
		case "kafka.consumer.messages.processed":
			processedSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range processedSum.DataPoints {
				processedCount += dp.Value
			}
		case "kafka.consumer.messages.committed":
			committedSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range committedSum.DataPoints {
				committedCount += dp.Value
			}
		}
	}

	// Verify both metrics were recorded with correct counts
	require.Equal(t, int64(2), processedCount, "should process 2 messages")
	require.Equal(t, int64(2), committedCount, "should commit 2 messages")
}

func TestAtMostOncePartitionRuntime_MetricsRecordProcessingFailures(t *testing.T) {
	// Setup metric reader to capture metrics
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = provider.Shutdown(context.Background())
	}()

	// Set global meter provider for this test
	otel.SetMeterProvider(provider)

	ctx := context.Background()

	processingErr := errors.New("processing failed")

	// Create test records
	records := []*kgo.Record{
		{Topic: "test-topic", Partition: 1, Offset: 200, Value: []byte("msg1")},
		{Topic: "test-topic", Partition: 1, Offset: 201, Value: []byte("msg2")},
	}

	// Setup mocks - processor will fail
	consumer := &mockFetchConsumer{
		fetches: []fetch{{records: records}},
	}
	processor := &concurrentSafeProcessor{
		err: processingErr,
	}
	acknowledger := &mockRecordAcknowledger{}

	// Create orchestrator and runtime
	orchestrator := newAtMostOnceOrchestrator("test-group", processor)
	runtime := orchestrator.Orchestrate(consumer, acknowledger)

	// Execute
	err := runtime.ProcessQueue(ctx)
	require.NoError(t, err) // Processing errors don't propagate in at-most-once

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(ctx, &rm)
	require.NoError(t, err)

	// Verify metrics were recorded
	require.Len(t, rm.ScopeMetrics, 1)
	metrics := rm.ScopeMetrics[0].Metrics

	// Find failure metric
	var failureCount int64
	for i := range metrics {
		if metrics[i].Name == "kafka.consumer.processing.failures" {
			failureSum := metrics[i].Data.(metricdata.Sum[int64])
			for _, dp := range failureSum.DataPoints {
				failureCount += dp.Value
			}
			break
		}
	}

	require.Equal(t, int64(2), failureCount, "should record 2 failures")
}
