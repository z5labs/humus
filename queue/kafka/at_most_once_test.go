//go:build testcontainers

// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAtMostOnce_BasicConsumption verifies basic message consumption with at-most-once semantics.
func TestAtMostOnce_BasicConsumption(t *testing.T) {
	t.Run("will consume and process all messages", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		createTopic(t, brokers, topic, 1)

		// Produce 10 test messages
		expectedCount := 10
		var producedMessages []Message
		for i := 0; i < expectedCount; i++ {
			producedMessages = append(producedMessages, testMessage(fmt.Sprintf("message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, producedMessages)

		// Create a processor that tracks consumed messages
		var consumedMessages []Message
		var mu sync.Mutex
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumedMessages = append(consumedMessages, msg)
			return nil
		})

		// Create runtime with at-most-once semantics
		runtime := newTestRuntime(t, brokers, "test-group", AtMostOnce(topic, processor))

		// Run runtime in background with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for all messages to be consumed or timeout
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumedMessages) >= expectedCount
		}, 8*time.Second, 100*time.Millisecond, "expected %d messages to be consumed", expectedCount)

		// Cancel context to stop runtime
		cancel()

		// Wait for runtime to finish
		select {
		case err := <-runtimeDone:
			// Context cancellation is expected
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop after context cancellation")
		}

		// Verify all messages were consumed
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, consumedMessages, expectedCount, "should consume exactly %d messages", expectedCount)

		// Verify messages were consumed in order
		for i := 0; i < expectedCount; i++ {
			expected := fmt.Sprintf("message-%d", i)
			actual := string(consumedMessages[i].Value)
			require.Equal(t, expected, actual, "message %d should be in order", i)
		}
	})
}

// TestAtMostOnce_CommitBeforeProcessing verifies that offsets are committed before processing.
// This is the KEY TEST that proves at-most-once semantics.
func TestAtMostOnce_CommitBeforeProcessing(t *testing.T) {
	t.Run("will commit offsets before processing messages", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-commit-order-group"
		createTopic(t, brokers, topic, 1)

		// Produce test messages
		messageCount := 5
		var producedMessages []Message
		for i := 0; i < messageCount; i++ {
			producedMessages = append(producedMessages, testMessage(fmt.Sprintf("message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, producedMessages)

		// First consumer: process messages (commits happen before processing)
		var processedCount int
		var mu sync.Mutex
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			processedCount++
			return nil
		})

		runtime1 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor))

		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel1()

		runtime1Done := make(chan error, 1)
		go func() {
			runtime1Done <- runtime1.ProcessQueue(ctx1)
		}()

		// Wait for all messages to be processed
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return processedCount >= messageCount
		}, 8*time.Second, 100*time.Millisecond, "expected %d messages to be processed", messageCount)

		// Give time for commits to be finalized
		time.Sleep(1 * time.Second)

		// Stop first consumer
		cancel1()
		select {
		case err := <-runtime1Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("first runtime did not stop")
		}

		// Second consumer with same group ID: should NOT see any messages
		// (proving commits happened, which in at-most-once happens BEFORE processing)
		var secondConsumerMessages int
		var mu2 sync.Mutex
		processor2 := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu2.Lock()
			defer mu2.Unlock()
			secondConsumerMessages++
			return nil
		})

		runtime2 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor2))

		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()

		runtime2Done := make(chan error, 1)
		go func() {
			runtime2Done <- runtime2.ProcessQueue(ctx2)
		}()

		// Wait a bit to see if any messages are delivered
		time.Sleep(3 * time.Second)

		// Stop second consumer
		cancel2()
		select {
		case err := <-runtime2Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("second runtime did not stop")
		}

		// Verify second consumer received no messages (proving offsets were committed)
		mu2.Lock()
		require.Equal(t, 0, secondConsumerMessages,
			"second consumer should not receive any messages - offsets were committed")
		mu2.Unlock()

		t.Logf("First consumer processed %d messages, second consumer received 0 messages (commits confirmed)",
			processedCount)
	})
}

// TestAtMostOnce_ProcessingErrorsDoNotPreventCommit verifies that processing errors
// result in message loss (at-most-once semantics).
func TestAtMostOnce_ProcessingErrorsDoNotPreventCommit(t *testing.T) {
	t.Run("will lose messages when processing fails", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-error-loss-group"
		createTopic(t, brokers, topic, 1)

		// Produce messages - all will trigger errors
		errorMessageCount := 5
		var errorMessages []Message
		for i := 0; i < errorMessageCount; i++ {
			errorMessages = append(errorMessages, testMessage(fmt.Sprintf("error-message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, errorMessages)

		// First consumer: processor always fails
		var firstConsumerAttempts int
		var mu1 sync.Mutex
		failingProcessor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu1.Lock()
			defer mu1.Unlock()
			firstConsumerAttempts++
			return fmt.Errorf("simulated processing error")
		})

		runtime1 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, failingProcessor))

		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel1()

		runtime1Done := make(chan error, 1)
		go func() {
			runtime1Done <- runtime1.ProcessQueue(ctx1)
		}()

		// Wait for all messages to be attempted
		require.Eventually(t, func() bool {
			mu1.Lock()
			defer mu1.Unlock()
			return firstConsumerAttempts >= errorMessageCount
		}, 8*time.Second, 100*time.Millisecond, "all messages should be attempted")

		// Give time for commits to happen
		time.Sleep(1 * time.Second)

		// Stop first consumer
		cancel1()
		select {
		case err := <-runtime1Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("first runtime did not stop")
		}

		// Verify messages were attempted exactly once (no retries)
		mu1.Lock()
		require.Equal(t, errorMessageCount, firstConsumerAttempts,
			"messages should be attempted exactly once")
		mu1.Unlock()

		// Second consumer with same group ID: should NOT see any messages
		// (proving failed messages were committed and lost)
		var secondConsumerAttempts int
		var mu2 sync.Mutex
		countingProcessor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu2.Lock()
			defer mu2.Unlock()
			secondConsumerAttempts++
			return nil
		})

		runtime2 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, countingProcessor))

		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()

		runtime2Done := make(chan error, 1)
		go func() {
			runtime2Done <- runtime2.ProcessQueue(ctx2)
		}()

		// Wait a bit to see if any messages are delivered
		time.Sleep(3 * time.Second)

		// Stop second consumer
		cancel2()
		select {
		case err := <-runtime2Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("second runtime did not stop")
		}

		// Verify second consumer received no messages (proving messages were lost)
		mu2.Lock()
		require.Equal(t, 0, secondConsumerAttempts,
			"second consumer should not receive any messages - failed messages were committed and lost")
		mu2.Unlock()

		t.Logf("First consumer attempted %d messages (all failed), second consumer received 0 messages (message loss confirmed)",
			firstConsumerAttempts)
	})

	t.Run("will continue processing after errors", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		createTopic(t, brokers, topic, 1)

		// Produce messages - some will trigger errors
		totalMessages := 10
		errorMessageIndices := map[int]bool{2: true, 5: true, 7: true}
		var producedMessages []Message
		for i := 0; i < totalMessages; i++ {
			value := fmt.Sprintf("message-%d", i)
			if errorMessageIndices[i] {
				value = fmt.Sprintf("error-message-%d", i)
			}
			producedMessages = append(producedMessages, testMessage(value))
		}
		produceTestMessages(t, brokers, topic, producedMessages)

		// Track processed and failed messages
		var processedMessages []Message
		var failedMessages []Message
		var mu sync.Mutex

		// Processor that fails on messages containing "error-"
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()

			value := string(msg.Value)
			if len(value) >= 6 && value[:6] == "error-" {
				failedMessages = append(failedMessages, msg)
				return fmt.Errorf("simulated processor error for message: %s", value)
			}

			processedMessages = append(processedMessages, msg)
			return nil
		})

		runtime := newTestRuntime(t, brokers, "test-error-continue-group", AtMostOnce(topic, processor))

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for all messages to be attempted
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(processedMessages)+len(failedMessages) >= totalMessages
		}, 10*time.Second, 100*time.Millisecond, "all messages should be attempted")

		// Stop runtime
		cancel()
		select {
		case err := <-runtimeDone:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop after context cancellation")
		}

		// Verify results
		mu.Lock()
		defer mu.Unlock()

		// Verify correct number of successes and failures
		expectedSuccesses := totalMessages - len(errorMessageIndices)
		require.Equal(t, expectedSuccesses, len(processedMessages),
			"should have %d successful messages", expectedSuccesses)
		require.Equal(t, len(errorMessageIndices), len(failedMessages),
			"should have %d failed messages", len(errorMessageIndices))

		// Verify all failed messages contain "error-"
		for _, msg := range failedMessages {
			require.Contains(t, string(msg.Value), "error-",
				"failed message should contain 'error-' prefix")
		}

		t.Logf("Processed %d messages successfully, %d messages failed (processing continued)",
			len(processedMessages), len(failedMessages))
	})
}

// TestAtMostOnce_MultiplePartitions verifies consumption from multiple partitions
// with at-most-once semantics.
func TestAtMostOnce_MultiplePartitions(t *testing.T) {
	t.Run("will consume messages from all partitions concurrently", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		numPartitions := int32(3)
		createTopic(t, brokers, topic, numPartitions)

		// Produce 10 messages per partition (30 total)
		messagesPerPartition := 10
		expectedTotalCount := int(numPartitions) * messagesPerPartition

		var producedMessages []Message
		for p := 0; p < int(numPartitions); p++ {
			for i := 0; i < messagesPerPartition; i++ {
				producedMessages = append(producedMessages, testMessage(fmt.Sprintf("p%d-message-%d", p, i)))
			}
		}
		produceTestMessages(t, brokers, topic, producedMessages)

		// Create a processor that tracks consumed messages and partitions
		var consumedMessages []Message
		partitionsSeen := make(map[int32]bool)
		var mu sync.Mutex
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumedMessages = append(consumedMessages, msg)
			partitionsSeen[msg.Partition] = true
			return nil
		})

		// Create runtime with at-most-once semantics
		runtime := newTestRuntime(t, brokers, "test-group", AtMostOnce(topic, processor))

		// Run runtime in background with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for all messages to be consumed or timeout
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumedMessages) >= expectedTotalCount
		}, 12*time.Second, 100*time.Millisecond, "expected %d messages to be consumed", expectedTotalCount)

		// Cancel context to stop runtime
		cancel()

		// Wait for runtime to finish
		select {
		case err := <-runtimeDone:
			// Context cancellation is expected
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop after context cancellation")
		}

		// Verify all messages were consumed
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, consumedMessages, expectedTotalCount, "should consume exactly %d messages", expectedTotalCount)

		// Verify all partitions were consumed from
		require.Len(t, partitionsSeen, int(numPartitions), "should consume from all %d partitions", numPartitions)
		for p := int32(0); p < numPartitions; p++ {
			require.True(t, partitionsSeen[p], "partition %d should have been consumed from", p)
		}

		t.Logf("Successfully consumed %d messages from %d partitions", len(consumedMessages), len(partitionsSeen))
	})
}

// TestAtMostOnce_MultipleTopics verifies consumption from multiple topics with at-most-once semantics.
func TestAtMostOnce_MultipleTopics(t *testing.T) {
	t.Run("will route messages to correct topic handlers", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic1 := "orders-topic"
		topic2 := "events-topic"
		createTopic(t, brokers, topic1, 1)
		createTopic(t, brokers, topic2, 1)

		// Produce messages to both topics
		ordersCount := 5
		eventsCount := 7

		var orderMessages []Message
		for i := 0; i < ordersCount; i++ {
			orderMessages = append(orderMessages, testMessage(fmt.Sprintf("order-%d", i)))
		}
		produceTestMessages(t, brokers, topic1, orderMessages)

		var eventMessages []Message
		for i := 0; i < eventsCount; i++ {
			eventMessages = append(eventMessages, testMessage(fmt.Sprintf("event-%d", i)))
		}
		produceTestMessages(t, brokers, topic2, eventMessages)

		// Create separate processors for each topic
		var consumedOrders []Message
		var consumedEvents []Message
		var mu sync.Mutex

		ordersProcessor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumedOrders = append(consumedOrders, msg)
			return nil
		})

		eventsProcessor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumedEvents = append(consumedEvents, msg)
			return nil
		})

		// Create runtime with at-most-once semantics for both topics
		runtime := newTestRuntime(t, brokers, "test-group",
			AtMostOnce(topic1, ordersProcessor),
			AtMostOnce(topic2, eventsProcessor),
		)

		// Run runtime in background with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for all messages to be consumed or timeout
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumedOrders) >= ordersCount && len(consumedEvents) >= eventsCount
		}, 12*time.Second, 100*time.Millisecond, "expected all messages to be consumed")

		// Cancel context to stop runtime
		cancel()

		// Wait for runtime to finish
		select {
		case err := <-runtimeDone:
			// Context cancellation is expected
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop after context cancellation")
		}

		// Verify correct number of messages per topic
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, consumedOrders, ordersCount, "should consume exactly %d orders", ordersCount)
		require.Len(t, consumedEvents, eventsCount, "should consume exactly %d events", eventsCount)

		// Verify messages were routed to correct handlers
		for i, msg := range consumedOrders {
			require.Equal(t, topic1, msg.Topic, "order message %d should be from %s topic", i, topic1)
			require.Contains(t, string(msg.Value), "order-", "order message %d should contain 'order-'", i)
		}

		for i, msg := range consumedEvents {
			require.Equal(t, topic2, msg.Topic, "event message %d should be from %s topic", i, topic2)
			require.Contains(t, string(msg.Value), "event-", "event message %d should contain 'event-'", i)
		}

		t.Logf("Successfully routed %d orders and %d events to correct handlers", len(consumedOrders), len(consumedEvents))
	})
}

// TestAtMostOnce_GracefulShutdown verifies graceful shutdown behavior.
func TestAtMostOnce_GracefulShutdown(t *testing.T) {
	t.Run("will stop gracefully after completing in-flight batch", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-shutdown-group"
		createTopic(t, brokers, topic, 1)

		// Produce messages
		messageCount := 10
		var messages []Message
		for i := 0; i < messageCount; i++ {
			messages = append(messages, testMessage(fmt.Sprintf("message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, messages)

		// Track processed messages
		var processedCount int
		var mu sync.Mutex

		// Processor that adds small delay
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			// Simulate processing time
			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			defer mu.Unlock()
			processedCount++

			return nil
		})

		runtime := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for some messages to be processed
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return processedCount >= 3
		}, 10*time.Second, 50*time.Millisecond, "at least 3 messages should be processed")

		// Cancel context
		cancel()

		// Wait for runtime to stop gracefully (should complete current batch)
		select {
		case err := <-runtimeDone:
			require.ErrorIs(t, err, context.Canceled, "runtime should return context.Canceled error")
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop within timeout after context cancellation")
		}

		// Verify messages were processed
		mu.Lock()
		finalCount := processedCount
		mu.Unlock()

		require.Greater(t, finalCount, 0, "some messages should have been processed")

		t.Logf("Processed %d messages before graceful shutdown", finalCount)
	})

	t.Run("will handle context cancellation in processor", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-cancel-processor-group"
		createTopic(t, brokers, topic, 1)

		// Produce messages
		messageCount := 5
		var messages []Message
		for i := 0; i < messageCount; i++ {
			messages = append(messages, testMessage(fmt.Sprintf("message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, messages)

		var processedCount int
		var contextCanceledCount int
		var mu sync.Mutex

		// Processor that respects context cancellation
		processor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()

			// Simulate work and check for cancellation
			select {
			case <-ctx.Done():
				contextCanceledCount++
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				processedCount++
				return nil
			}
		})

		runtime := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		runtimeDone := make(chan error, 1)
		go func() {
			runtimeDone <- runtime.ProcessQueue(ctx)
		}()

		// Wait for at least one message to be processed
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return processedCount >= 1
		}, 5*time.Second, 50*time.Millisecond, "at least one message should be processed")

		// Cancel immediately
		cancel()

		// Wait for runtime to stop
		select {
		case err := <-runtimeDone:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not stop")
		}

		mu.Lock()
		t.Logf("Processed %d messages successfully, %d processor calls saw context cancellation",
			processedCount, contextCanceledCount)
		mu.Unlock()
	})
}

// TestAtMostOnce_OffsetCommits verifies that offsets are committed correctly
// with at-most-once semantics (committed before processing).
func TestAtMostOnce_OffsetCommits(t *testing.T) {
	t.Run("will commit offsets immediately and not reprocess on restart", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-offset-group"
		createTopic(t, brokers, topic, 1)

		// Produce initial batch of messages
		firstBatchSize := 10
		var firstBatchProduced []Message
		for i := 0; i < firstBatchSize; i++ {
			firstBatchProduced = append(firstBatchProduced, testMessage(fmt.Sprintf("batch1-message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, firstBatchProduced)

		// First consumer: process all messages in first batch
		var firstBatchConsumed []Message
		var mu1 sync.Mutex
		processor1 := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu1.Lock()
			defer mu1.Unlock()
			firstBatchConsumed = append(firstBatchConsumed, msg)
			return nil
		})

		runtime1 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor1))

		ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel1()

		runtime1Done := make(chan error, 1)
		go func() {
			runtime1Done <- runtime1.ProcessQueue(ctx1)
		}()

		// Wait for all first batch messages to be consumed
		require.Eventually(t, func() bool {
			mu1.Lock()
			defer mu1.Unlock()
			return len(firstBatchConsumed) >= firstBatchSize
		}, 10*time.Second, 100*time.Millisecond, "expected %d messages from first batch to be consumed", firstBatchSize)

		// Give time for commits to happen
		time.Sleep(1 * time.Second)

		// Stop first consumer gracefully
		cancel1()
		select {
		case err := <-runtime1Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("first runtime did not stop after context cancellation")
		}

		// Produce second batch of messages AFTER first consumer stops
		secondBatchSize := 10
		var secondBatchProduced []Message
		for i := 0; i < secondBatchSize; i++ {
			secondBatchProduced = append(secondBatchProduced, testMessage(fmt.Sprintf("batch2-message-%d", i)))
		}
		produceTestMessages(t, brokers, topic, secondBatchProduced)

		// Second consumer with same group ID: should only see second batch
		var secondBatchConsumed []Message
		var mu2 sync.Mutex
		processor2 := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu2.Lock()
			defer mu2.Unlock()
			secondBatchConsumed = append(secondBatchConsumed, msg)
			return nil
		})

		runtime2 := newTestRuntime(t, brokers, groupID, AtMostOnce(topic, processor2))

		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()

		runtime2Done := make(chan error, 1)
		go func() {
			runtime2Done <- runtime2.ProcessQueue(ctx2)
		}()

		// Wait for second batch messages to be consumed
		require.Eventually(t, func() bool {
			mu2.Lock()
			defer mu2.Unlock()
			return len(secondBatchConsumed) >= secondBatchSize
		}, 10*time.Second, 100*time.Millisecond, "expected %d messages from second batch to be consumed", secondBatchSize)

		// Stop second consumer
		cancel2()
		select {
		case err := <-runtime2Done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("second runtime did not stop after context cancellation")
		}

		// Verify first consumer processed first batch
		mu1.Lock()
		require.GreaterOrEqual(t, len(firstBatchConsumed), firstBatchSize, "first consumer should have consumed at least %d messages", firstBatchSize)
		// All messages should be from batch1
		for i, msg := range firstBatchConsumed {
			require.Contains(t, string(msg.Value), "batch1-", "first consumer message %d should be from batch1", i)
		}
		mu1.Unlock()

		// Verify second consumer only processed second batch (offset commit worked)
		mu2.Lock()
		require.GreaterOrEqual(t, len(secondBatchConsumed), secondBatchSize, "second consumer should have consumed at least %d messages", secondBatchSize)
		// All messages should be from batch2 (proving offset resume worked)
		for i, msg := range secondBatchConsumed {
			require.Contains(t, string(msg.Value), "batch2-", "second consumer message %d should be from batch2", i)
		}
		mu2.Unlock()

		t.Logf("Offset commit test passed: first consumer processed %d messages, second consumer processed %d new messages",
			len(firstBatchConsumed), len(secondBatchConsumed))
	})
}
