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

//
// Core Functionality Tests
//

// TestBasicConsumption verifies basic message consumption with at-least-once semantics.
func TestBasicConsumption(t *testing.T) {
	// Start container once for all subtests
	brokers, cleanup := setupKafkaContainer(t)
	defer cleanup()

	t.Run("will consume and process all messages in order", func(t *testing.T) {
		topic := "basic-consumption-ordered"
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

		// Create runtime with at-least-once semantics
		runtime, err := newTestRuntime(t, brokers, "basic-consumption-group", []TopicProcessor{
			{Topic: topic, Processor: processor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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

	t.Run("will consume messages from all partitions", func(t *testing.T) {
		topic := "basic-consumption-partitions"
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

		// Create runtime with at-least-once semantics
		runtime, err := newTestRuntime(t, brokers, "partitions-group", []TopicProcessor{
			{Topic: topic, Processor: processor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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
	})

	t.Run("will route messages to correct topic handlers", func(t *testing.T) {
		topic1 := "basic-consumption-orders"
		topic2 := "basic-consumption-events"
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

		// Create runtime with at-least-once semantics for both topics
		runtime, err := newTestRuntime(t, brokers, "multi-topic-group", []TopicProcessor{
			{Topic: topic1, Processor: ordersProcessor, DeliveryMode: AtLeastOnce},
			{Topic: topic2, Processor: eventsProcessor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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
	})
}

// ProcessorFunc is a function adapter for the Processor interface.
type ProcessorFunc func(context.Context, Message) error

func (f ProcessorFunc) Process(ctx context.Context, msg Message) error {
	return f(ctx, msg)
}

//
// Consumer Group Behavior Tests
//

// TestOffsetCommits verifies that consumer offsets are committed and resumed correctly.
func TestOffsetCommits(t *testing.T) {
	t.Run("will resume from committed offset after restart", func(t *testing.T) {
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

		runtime1, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor1, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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

		runtime2, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor2, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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
	})
}

// TestRebalancing verifies consumer group rebalancing behavior.
func TestRebalancing(t *testing.T) {
	t.Run("will rebalance partitions when second consumer joins", func(t *testing.T) {
		brokers, cleanup := setupKafkaContainer(t)
		defer cleanup()

		topic := "test-topic"
		groupID := "test-rebalance-group"
		numPartitions := int32(3)
		createTopic(t, brokers, topic, numPartitions)

		// Produce initial batch of messages
		initialMessagesPerPartition := 10
		initialTotalMessages := int(numPartitions) * initialMessagesPerPartition
		var initialMessages []Message
		for p := 0; p < int(numPartitions); p++ {
			for i := 0; i < initialMessagesPerPartition; i++ {
				initialMessages = append(initialMessages, testMessage(fmt.Sprintf("initial-p%d-msg-%d", p, i)))
			}
		}
		produceTestMessages(t, brokers, topic, initialMessages)

		// Track messages consumed by each consumer
		var consumer1Messages []Message
		var consumer2Messages []Message
		consumer1Partitions := make(map[int32]bool)
		consumer2Partitions := make(map[int32]bool)
		var mu sync.Mutex

		processor1 := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumer1Messages = append(consumer1Messages, msg)
			consumer1Partitions[msg.Partition] = true
			return nil
		})

		processor2 := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu.Lock()
			defer mu.Unlock()
			consumer2Messages = append(consumer2Messages, msg)
			consumer2Partitions[msg.Partition] = true
			return nil
		})

		// Start first consumer
		runtime1, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor1, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel1()

		runtime1Done := make(chan error, 1)
		go func() {
			runtime1Done <- runtime1.ProcessQueue(ctx1)
		}()

		// Wait for first consumer to start processing
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumer1Messages) > 0
		}, 10*time.Second, 100*time.Millisecond, "first consumer should start processing")

		// Give first consumer time to get all partitions
		time.Sleep(2 * time.Second)

		// Start second consumer (triggers rebalance)
		runtime2, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor2, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()

		runtime2Done := make(chan error, 1)
		go func() {
			runtime2Done <- runtime2.ProcessQueue(ctx2)
		}()

		// Wait for rebalance to complete before producing more messages
		// Rebalancing involves: consumer joining, partitions being revoked from consumer1,
		// partitions being assigned to both consumers. This can take a few seconds.
		// Without this delay, consumer1 might consume all the second batch messages
		// before consumer2 gets assigned any partitions.
		time.Sleep(5 * time.Second)
		// Produce second batch of messages after second consumer joins
		// This ensures both consumers will have messages to process
		secondMessagesPerPartition := 10
		secondTotalMessages := int(numPartitions) * secondMessagesPerPartition
		var secondMessages []Message
		for p := 0; p < int(numPartitions); p++ {
			for i := 0; i < secondMessagesPerPartition; i++ {
				secondMessages = append(secondMessages, testMessage(fmt.Sprintf("second-p%d-msg-%d", p, i)))
			}
		}
		produceTestMessages(t, brokers, topic, secondMessages)

		totalMessages := initialTotalMessages + secondTotalMessages

		// Wait for second consumer to start processing (rebalance complete)
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumer2Messages) > 0
		}, 15*time.Second, 100*time.Millisecond, "second consumer should start processing after rebalance")

		// Wait for all messages to be consumed
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(consumer1Messages)+len(consumer2Messages) >= totalMessages
		}, 20*time.Second, 200*time.Millisecond, "all messages should be consumed by both consumers")

		// Stop both consumers
		cancel1()
		cancel2()

		select {
		case <-runtime1Done:
		case <-time.After(5 * time.Second):
			t.Fatal("runtime1 did not stop")
		}

		select {
		case <-runtime2Done:
		case <-time.After(5 * time.Second):
			t.Fatal("runtime2 did not stop")
		}

		// Verify results
		mu.Lock()
		defer mu.Unlock()

		// Both consumers should have processed messages
		require.Greater(t, len(consumer1Messages), 0, "consumer1 should have processed messages")
		require.Greater(t, len(consumer2Messages), 0, "consumer2 should have processed messages")

		// Total messages consumed should be at least totalMessages (may have duplicates due to rebalance)
		require.GreaterOrEqual(t, len(consumer1Messages)+len(consumer2Messages), totalMessages,
			"total messages consumed should be at least %d", totalMessages)

		// Verify partitions were distributed (not necessarily evenly, but both should have some)
		t.Logf("Consumer1 processed %d messages from partitions: %v", len(consumer1Messages), mapKeysToSlice(consumer1Partitions))
		t.Logf("Consumer2 processed %d messages from partitions: %v", len(consumer2Messages), mapKeysToSlice(consumer2Partitions))

		// At least one consumer should have seen multiple partitions or both should have seen partitions
		totalPartitionsSeen := len(consumer1Partitions) + len(consumer2Partitions)
		require.GreaterOrEqual(t, totalPartitionsSeen, int(numPartitions),
			"consumers should collectively see all %d partitions", numPartitions)
	})
}

// Helper function to convert map keys to slice
func mapKeysToSlice(m map[int32]bool) []int32 {
	keys := make([]int32, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ==================== Error Handling & Recovery Tests ====================

// TestErrorHandling verifies error handling and graceful shutdown behavior.
func TestErrorHandling(t *testing.T) {
	// Start container once for all subtests
	brokers, cleanup := setupKafkaContainer(t)
	defer cleanup()

	t.Run("will continue processing after processor errors", func(t *testing.T) {
		topic := "error-handling-continue"
		groupID := "error-continue-group"
		createTopic(t, brokers, topic, 1)

		// Produce messages - some will trigger errors
		totalMessages := 10
		errorMessageIndices := map[int]bool{2: true, 5: true, 7: true} // Messages that should fail
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

		runtime, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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

		t.Logf("Processed %d messages successfully, %d messages failed",
			len(processedMessages), len(failedMessages))
	})

	t.Run("will commit all messages including failed ones", func(t *testing.T) {
		topic := "error-handling-commits"
		groupID := "error-commits-group"
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
			return fmt.Errorf("simulated error")
		})

		runtime1, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: failingProcessor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel1()

		runtime1Done := make(chan error, 1)
		go func() {
			runtime1Done <- runtime1.ProcessQueue(ctx1)
		}()

		// Wait for all messages to be attempted at least once
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

		// Second consumer with same group ID: should NOT see any messages (all were committed despite errors)
		var secondConsumerAttempts int
		var mu2 sync.Mutex
		countingProcessor := ProcessorFunc(func(ctx context.Context, msg Message) error {
			mu2.Lock()
			defer mu2.Unlock()
			secondConsumerAttempts++
			return nil
		})

		runtime2, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: countingProcessor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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

		// Verify second consumer received no messages (proving failed messages were committed)
		mu2.Lock()
		require.Equal(t, 0, secondConsumerAttempts,
			"second consumer should not receive any messages - failed messages were committed")
		mu2.Unlock()

		t.Logf("First consumer attempted %d messages (all failed), second consumer received 0 messages (commits worked)",
			firstConsumerAttempts)
	})

	t.Run("will stop gracefully after completing in-flight batch", func(t *testing.T) {
		topic := "error-handling-shutdown"
		groupID := "shutdown-group"
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

		runtime, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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
		topic := "error-handling-cancel"
		groupID := "cancel-processor-group"
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

		runtime, err := newTestRuntime(t, brokers, groupID, []TopicProcessor{
			{Topic: topic, Processor: processor, DeliveryMode: AtLeastOnce},
		})
		require.NoError(t, err)

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
