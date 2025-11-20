// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecorder(t *testing.T) {
	t.Run("will initialize metrics recorder", func(t *testing.T) {
		// Setup a metric reader to capture metrics
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otel.SetMeterProvider(provider)
		defer otel.SetMeterProvider(nil)

		// Create metrics recorder
		recorder, err := newMetricsRecorder()
		require.NoError(t, err)
		require.NotNil(t, recorder)
		require.NotNil(t, recorder.messagesProcessed)
		require.NotNil(t, recorder.messagesCommitted)
		require.NotNil(t, recorder.processingFailures)
	})

	t.Run("will record messages processed", func(t *testing.T) {
		// Setup a metric reader to capture metrics
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otel.SetMeterProvider(provider)
		defer otel.SetMeterProvider(nil)

		// Create metrics recorder
		recorder, err := newMetricsRecorder()
		require.NoError(t, err)

		// Record some messages processed
		ctx := context.Background()
		recorder.recordMessageProcessed(ctx, "test-topic", 0, "at_least_once")
		recorder.recordMessageProcessed(ctx, "test-topic", 1, "at_least_once")
		recorder.recordMessageProcessed(ctx, "test-topic", 0, "at_most_once")

		// Collect metrics
		var rm metricdata.ResourceMetrics
		err = reader.Collect(ctx, &rm)
		require.NoError(t, err)

		// Verify metrics
		require.Len(t, rm.ScopeMetrics, 1)
		require.Equal(t, meterName, rm.ScopeMetrics[0].Scope.Name)

		// Find the messages processed metric
		var found bool
		for _, metric := range rm.ScopeMetrics[0].Metrics {
			if metric.Name == "kafka.consumer.messages.processed" {
				found = true
				require.Equal(t, "{message}", metric.Unit)
				require.Equal(t, "Total number of Kafka messages processed", metric.Description)

				// Check that it's a sum
				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.True(t, sum.IsMonotonic)

				// We should have recorded 3 data points (different label combinations)
				require.Len(t, sum.DataPoints, 3)

				// Verify total count
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				require.Equal(t, int64(3), total)
			}
		}
		require.True(t, found, "messages processed metric should be present")
	})

	t.Run("will record messages committed", func(t *testing.T) {
		// Setup a metric reader to capture metrics
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otel.SetMeterProvider(provider)
		defer otel.SetMeterProvider(nil)

		// Create metrics recorder
		recorder, err := newMetricsRecorder()
		require.NoError(t, err)

		// Record some commits
		ctx := context.Background()
		recorder.recordMessagesCommitted(ctx, "test-topic", 0, 10)
		recorder.recordMessagesCommitted(ctx, "test-topic", 1, 5)

		// Collect metrics
		var rm metricdata.ResourceMetrics
		err = reader.Collect(ctx, &rm)
		require.NoError(t, err)

		// Find the messages committed metric
		var found bool
		for _, metric := range rm.ScopeMetrics[0].Metrics {
			if metric.Name == "kafka.consumer.messages.committed" {
				found = true
				require.Equal(t, "{message}", metric.Unit)
				require.Equal(t, "Total number of Kafka messages committed", metric.Description)

				// Check that it's a sum
				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.True(t, sum.IsMonotonic)

				// We should have recorded 2 data points (different partitions)
				require.Len(t, sum.DataPoints, 2)

				// Verify total count
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				require.Equal(t, int64(15), total)
			}
		}
		require.True(t, found, "messages committed metric should be present")
	})

	t.Run("will record processing failures", func(t *testing.T) {
		// Setup a metric reader to capture metrics
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otel.SetMeterProvider(provider)
		defer otel.SetMeterProvider(nil)

		// Create metrics recorder
		recorder, err := newMetricsRecorder()
		require.NoError(t, err)

		// Record some failures
		ctx := context.Background()
		recorder.recordProcessingFailure(ctx, "test-topic", 0, "at_least_once")
		recorder.recordProcessingFailure(ctx, "test-topic", 0, "at_least_once")
		recorder.recordProcessingFailure(ctx, "test-topic", 1, "at_most_once")

		// Collect metrics
		var rm metricdata.ResourceMetrics
		err = reader.Collect(ctx, &rm)
		require.NoError(t, err)

		// Find the processing failures metric
		var found bool
		for _, metric := range rm.ScopeMetrics[0].Metrics {
			if metric.Name == "kafka.consumer.processing.failures" {
				found = true
				require.Equal(t, "{failure}", metric.Unit)
				require.Equal(t, "Total number of Kafka message processing failures", metric.Description)

				// Check that it's a sum
				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.True(t, sum.IsMonotonic)

				// We should have recorded 2 data points (different label combinations)
				require.Len(t, sum.DataPoints, 2)

				// Verify total count
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				require.Equal(t, int64(3), total)
			}
		}
		require.True(t, found, "processing failures metric should be present")
	})

	t.Run("will include correct attributes", func(t *testing.T) {
		// Setup a metric reader to capture metrics
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otel.SetMeterProvider(provider)
		defer otel.SetMeterProvider(nil)

		// Create metrics recorder
		recorder, err := newMetricsRecorder()
		require.NoError(t, err)

		// Record a message with specific attributes
		ctx := context.Background()
		recorder.recordMessageProcessed(ctx, "my-topic", 42, "at_least_once")

		// Collect metrics
		var rm metricdata.ResourceMetrics
		err = reader.Collect(ctx, &rm)
		require.NoError(t, err)

		// Find the messages processed metric
		for _, metric := range rm.ScopeMetrics[0].Metrics {
			if metric.Name == "kafka.consumer.messages.processed" {
				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.Len(t, sum.DataPoints, 1)

				// Verify attributes
				attrs := sum.DataPoints[0].Attributes
				topic, found := attrs.Value("topic")
				require.True(t, found)
				require.Equal(t, "my-topic", topic.AsString())

				partition, found := attrs.Value("partition")
				require.True(t, found)
				require.Equal(t, int64(42), partition.AsInt64())

				deliverySemantics, found := attrs.Value("delivery_semantics")
				require.True(t, found)
				require.Equal(t, "at_least_once", deliverySemantics.AsString())
			}
		}
	})
}
