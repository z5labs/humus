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
	"time"

	"github.com/sourcegraph/conc/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Test helpers and mocks

// captureHandler captures log records for testing
type captureHandler struct {
	slog.Handler
	records []slog.Record
	mu      sync.Mutex
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record)
	return nil
}

func (h *captureHandler) getRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record{}, h.records...)
}

// pollFetcherFunc is a function type mock for pollFetcher interface
type pollFetcherFunc func(context.Context) kgo.Fetches

func (f pollFetcherFunc) PollFetches(ctx context.Context) kgo.Fetches {
	return f(ctx)
}

func (f pollFetcherFunc) Close() {}

// recordsHandlerFunc is a function type mock for recordsHandler interface
type recordsHandlerFunc func(context.Context, []*kgo.Record) error

func (f recordsHandlerFunc) Handle(ctx context.Context, records []*kgo.Record) error {
	return f(ctx, records)
}

// recordsCommitterFunc is a function type mock for recordsCommitter interface
type recordsCommitterFunc func(context.Context, ...*kgo.Record) error

func (f recordsCommitterFunc) CommitRecords(ctx context.Context, records ...*kgo.Record) error {
	return f(ctx, records...)
}

// callTracker tracks method calls for verification
type callTracker struct {
	calls []string
	mu    sync.Mutex
}

func (t *callTracker) record(method string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, method)
}

func (t *callTracker) getCalls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.calls...)
}

// Helper functions

func makeTestLogger(capture *captureHandler) *slog.Logger {
	return slog.New(capture)
}

func requireChannelClosed(t *testing.T, ch <-chan []*kgo.Record) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for channel to close")
	}
}

func requireGoroutineCompletes(t *testing.T, fn func(), timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(timeout):
		t.Fatal("goroutine did not complete within timeout")
	}
}

// Context Cancellation Tests

func TestEventLoop_Tick_ContextCancellation(t *testing.T) {
	t.Run("will return ctx.Err()", func(t *testing.T) {
		t.Run("if context is already cancelled", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Cancel immediately

			loop := eventLoop{
				log:                slog.Default(),
				fetches:            make(chan kgo.FetchTopic),
				assignedPartitions: make(chan assignedPartition),
				lostPartitions:     make(chan topicPartition),
				revokedPartitions:  make(chan topicPartition),
				topicHandlers:      make(map[string]func(recordsCommitter) recordsHandler),
				topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
				partitionPool:      pool.New().WithContext(ctx),
			}

			err := loop.tick(ctx)
			require.ErrorIs(t, err, context.Canceled)
		})

		t.Run("if context is cancelled during select", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			loop := eventLoop{
				log:                slog.Default(),
				fetches:            make(chan kgo.FetchTopic),
				assignedPartitions: make(chan assignedPartition),
				lostPartitions:     make(chan topicPartition),
				revokedPartitions:  make(chan topicPartition),
				topicHandlers:      make(map[string]func(recordsCommitter) recordsHandler),
				topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
				partitionPool:      pool.New().WithContext(ctx),
			}

			err := loop.tick(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})
}

func TestEventLoop_Run_ContextCancellation(t *testing.T) {
	t.Run("will exit and shutdown", func(t *testing.T) {
		t.Run("when context is cancelled", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())

			loop := eventLoop{
				log:                slog.Default(),
				fetches:            make(chan kgo.FetchTopic),
				assignedPartitions: make(chan assignedPartition),
				lostPartitions:     make(chan topicPartition),
				revokedPartitions:  make(chan topicPartition),
				topicHandlers:      make(map[string]func(recordsCommitter) recordsHandler),
				topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
				partitionPool:      pool.New().WithContext(ctx),
			}

			// Cancel after a short delay
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()

			err := loop.run(ctx)
			// run() returns the result of shutdown(), which is nil when no partitions are active
			require.NoError(t, err)
		})
	})
}

func TestEventLoop_HandleFetch_ContextCancellation(t *testing.T) {
	t.Run("will return ctx.Err()", func(t *testing.T) {
		t.Run("when context is cancelled during send", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Cancel immediately

			// Create partition channel but don't read from it
			partitionCh := make(chan []*kgo.Record)

			loop := eventLoop{
				log: slog.Default(),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					{topic: "test-topic", partition: 0}: partitionCh,
				},
			}

			fetch := kgo.FetchTopic{
				Topic: "test-topic",
				Partitions: []kgo.FetchPartition{
					{
						Partition: 0,
						Records:   []*kgo.Record{{Topic: "test-topic"}},
					},
				},
			}

			err := loop.handleFetch(ctx, fetch)
			require.ErrorIs(t, err, context.Canceled)
		})
	})
}

// Shutdown Tests

func TestEventLoop_Shutdown_NoPartitions(t *testing.T) {
	t.Run("will shutdown cleanly", func(t *testing.T) {
		t.Run("when no partitions are assigned", func(t *testing.T) {
			ctx := context.Background()
			loop := eventLoop{
				log:             slog.Default(),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
				partitionPool:   pool.New().WithContext(ctx),
			}

			err := loop.shutdown()
			require.NoError(t, err)
		})
	})
}

func TestEventLoop_Shutdown_ActivePartitions(t *testing.T) {
	t.Run("will close all partition channels", func(t *testing.T) {
		t.Run("and wait for goroutines to complete", func(t *testing.T) {
			ctx := context.Background()

			ch1 := make(chan []*kgo.Record)
			ch2 := make(chan []*kgo.Record)
			ch3 := make(chan []*kgo.Record)

			loop := eventLoop{
				log: slog.Default(),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					{topic: "topic1", partition: 0}: ch1,
					{topic: "topic2", partition: 1}: ch2,
					{topic: "topic3", partition: 2}: ch3,
				},
				partitionPool: pool.New().WithContext(ctx),
			}

			// Spawn goroutines that read from channels
			for _, ch := range loop.topicPartitions {
				ch := ch // Capture for goroutine
				loop.partitionPool.Go(func(ctx context.Context) error {
					for {
						select {
						case _, ok := <-ch:
							if !ok {
								return nil
							}
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				})
			}

			// Shutdown should close all channels and wait
			err := loop.shutdown()
			require.NoError(t, err)

			// Verify all channels are closed
			requireChannelClosed(t, ch1)
			requireChannelClosed(t, ch2)
			requireChannelClosed(t, ch3)
		})
	})
}

func TestEventLoop_Shutdown_WaitsForGoroutines(t *testing.T) {
	t.Run("will block until partition goroutines complete", func(t *testing.T) {
		t.Run("even if handler is slow", func(t *testing.T) {
			ctx := context.Background()

			partitionCh := make(chan []*kgo.Record)
			handlerCompleted := make(chan struct{})

			loop := eventLoop{
				log: slog.Default(),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					{topic: "test-topic", partition: 0}: partitionCh,
				},
				partitionPool: pool.New().WithContext(ctx),
			}

			// Spawn goroutine that takes time to complete
			loop.partitionPool.Go(func(ctx context.Context) error {
				defer close(handlerCompleted)
				for {
					select {
					case _, ok := <-partitionCh:
						if !ok {
							time.Sleep(100 * time.Millisecond) // Simulate slow cleanup
							return nil
						}
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			})

			// Shutdown should wait for the handler
			startTime := time.Now()
			err := loop.shutdown()
			duration := time.Since(startTime)

			require.NoError(t, err)
			require.GreaterOrEqual(t, duration, 100*time.Millisecond, "shutdown should have waited for handler")

			// Verify handler actually completed
			select {
			case <-handlerCompleted:
				// Success
			case <-time.After(50 * time.Millisecond):
				t.Fatal("handler did not complete")
			}
		})
	})
}

// Partition Lifecycle Tests

func TestEventLoop_HandleAssignedPartition(t *testing.T) {
	t.Run("will create channel and spawn goroutine", func(t *testing.T) {
		t.Run("when partition is assigned", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
				return nil
			})

			loop := eventLoop{
				log:             makeTestLogger(capture),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
				partitionPool:   pool.New().WithContext(ctx),
			}

			ap := assignedPartition{
				topicPartition: topicPartition{topic: "test-topic", partition: 0},
				handler:        handler,
			}

			err := loop.handleAssignedPartition(ctx, ap)
			require.NoError(t, err)

			// Verify channel was created
			ch, exists := loop.topicPartitions[ap.topicPartition]
			require.True(t, exists, "partition channel should exist")
			require.NotNil(t, ch, "partition channel should not be nil")

			// Verify log message
			records := capture.getRecords()
			require.Len(t, records, 1)
			require.Equal(t, "topic partition assigned", records[0].Message)

			// Verify attributes in log
			var foundTopic, foundPartition bool
			records[0].Attrs(func(a slog.Attr) bool {
				if a.Key == "messaging.destination.name" && a.Value.String() == "test-topic" {
					foundTopic = true
				}
				if a.Key == "messaging.destination.partition.id" && a.Value.Int64() == 0 {
					foundPartition = true
				}
				return true
			})
			require.True(t, foundTopic, "log should contain topic attribute")
			require.True(t, foundPartition, "log should contain partition attribute")
		})

		t.Run("and handler receives records sent to channel", func(t *testing.T) {
			ctx := context.Background()

			var receivedRecords []*kgo.Record
			recordsReceived := make(chan struct{})

			handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
				receivedRecords = records
				close(recordsReceived)
				return nil
			})

			loop := eventLoop{
				log:             slog.Default(),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
				partitionPool:   pool.New().WithContext(ctx),
			}

			ap := assignedPartition{
				topicPartition: topicPartition{topic: "test-topic", partition: 0},
				handler:        handler,
			}

			err := loop.handleAssignedPartition(ctx, ap)
			require.NoError(t, err)

			// Send records to the channel
			testRecords := []*kgo.Record{{Topic: "test-topic", Partition: 0}}
			ch := loop.topicPartitions[ap.topicPartition]
			ch <- testRecords

			// Wait for handler to receive
			select {
			case <-recordsReceived:
				require.Equal(t, testRecords, receivedRecords)
			case <-time.After(500 * time.Millisecond):
				t.Fatal("handler did not receive records")
			}
		})
	})
}

func TestEventLoop_HandleRevokedPartition(t *testing.T) {
	t.Run("will close channel and remove from map", func(t *testing.T) {
		t.Run("when partition exists", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			partitionCh := make(chan []*kgo.Record)
			tp := topicPartition{topic: "test-topic", partition: 0}

			loop := eventLoop{
				log: makeTestLogger(capture),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					tp: partitionCh,
				},
			}

			err := loop.handleRevokedPartition(ctx, tp)
			require.NoError(t, err)

			// Verify channel is closed
			requireChannelClosed(t, partitionCh)

			// Verify removed from map
			_, exists := loop.topicPartitions[tp]
			require.False(t, exists, "partition should be removed from map")

			// Verify log message
			records := capture.getRecords()
			require.Len(t, records, 1)
			require.Equal(t, "topic partition revoked", records[0].Message)
		})

		t.Run("and log warning when partition does not exist", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			loop := eventLoop{
				log:             makeTestLogger(capture),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
			}

			tp := topicPartition{topic: "unknown-topic", partition: 99}

			err := loop.handleRevokedPartition(ctx, tp)
			require.NoError(t, err)

			// Verify warning log
			records := capture.getRecords()
			require.Len(t, records, 2)
			require.Equal(t, "topic partition revoked", records[0].Message)
			require.Equal(t, "topic partition not found for revoked partition", records[1].Message)
		})
	})
}

func TestEventLoop_HandleLostPartition(t *testing.T) {
	t.Run("will close channel and remove from map", func(t *testing.T) {
		t.Run("when partition exists", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			partitionCh := make(chan []*kgo.Record)
			tp := topicPartition{topic: "test-topic", partition: 0}

			loop := eventLoop{
				log: makeTestLogger(capture),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					tp: partitionCh,
				},
			}

			err := loop.handleLostPartition(ctx, tp)
			require.NoError(t, err)

			// Verify channel is closed
			requireChannelClosed(t, partitionCh)

			// Verify removed from map
			_, exists := loop.topicPartitions[tp]
			require.False(t, exists, "partition should be removed from map")

			// Verify log message
			records := capture.getRecords()
			require.Len(t, records, 1)
			require.Equal(t, "topic partition lost", records[0].Message)
		})

		t.Run("and log warning when partition does not exist", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			loop := eventLoop{
				log:             makeTestLogger(capture),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
			}

			tp := topicPartition{topic: "unknown-topic", partition: 99}

			err := loop.handleLostPartition(ctx, tp)
			require.NoError(t, err)

			// Verify warning log
			records := capture.getRecords()
			require.Len(t, records, 2)
			require.Equal(t, "topic partition lost", records[0].Message)
			require.Equal(t, "topic partition not found for lost partition", records[1].Message)
		})
	})
}

func TestEventLoop_HandleFetch(t *testing.T) {
	t.Run("will route records to correct partition channel", func(t *testing.T) {
		t.Run("when partition is assigned", func(t *testing.T) {
			ctx := context.Background()

			ch1 := make(chan []*kgo.Record, 1)
			ch2 := make(chan []*kgo.Record, 1)

			loop := eventLoop{
				log: slog.Default(),
				topicPartitions: map[topicPartition]chan []*kgo.Record{
					{topic: "test-topic", partition: 0}: ch1,
					{topic: "test-topic", partition: 1}: ch2,
				},
			}

			testRecords1 := []*kgo.Record{{Topic: "test-topic", Partition: 0}}
			testRecords2 := []*kgo.Record{{Topic: "test-topic", Partition: 1}}

			fetch := kgo.FetchTopic{
				Topic: "test-topic",
				Partitions: []kgo.FetchPartition{
					{Partition: 0, Records: testRecords1},
					{Partition: 1, Records: testRecords2},
				},
			}

			err := loop.handleFetch(ctx, fetch)
			require.NoError(t, err)

			// Verify records sent to correct channels
			select {
			case records := <-ch1:
				require.Equal(t, testRecords1, records)
			case <-time.After(100 * time.Millisecond):
				t.Fatal("records not sent to channel 1")
			}

			select {
			case records := <-ch2:
				require.Equal(t, testRecords2, records)
			case <-time.After(100 * time.Millisecond):
				t.Fatal("records not sent to channel 2")
			}
		})

		t.Run("and log warning when partition not found", func(t *testing.T) {
			ctx := context.Background()
			capture := &captureHandler{Handler: slog.Default().Handler()}

			loop := eventLoop{
				log:             makeTestLogger(capture),
				topicPartitions: make(map[topicPartition]chan []*kgo.Record),
			}

			fetch := kgo.FetchTopic{
				Topic: "test-topic",
				Partitions: []kgo.FetchPartition{
					{Partition: 99, Records: []*kgo.Record{{Topic: "test-topic"}}},
				},
			}

			err := loop.handleFetch(ctx, fetch)
			require.NoError(t, err)

			// Verify warning log
			records := capture.getRecords()
			require.Len(t, records, 1)
			require.Equal(t, "topic partition not found for fetched records", records[0].Message)
		})
	})
}

// Integration Tests

func TestEventLoop_Run_FullLifecycle(t *testing.T) {
	t.Run("will handle assign -> fetch -> revoke -> cancel sequence", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		tracker := &callTracker{}
		handlerCalled := make(chan struct{}, 1)

		handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
			tracker.record("Handle")
			select {
			case handlerCalled <- struct{}{}:
			default:
			}
			return nil
		})

		loop := eventLoop{
			log:                slog.Default(),
			fetches:            make(chan kgo.FetchTopic, 1),
			assignedPartitions: make(chan assignedPartition, 1),
			lostPartitions:     make(chan topicPartition, 1),
			revokedPartitions:  make(chan topicPartition, 1),
			topicHandlers:      make(map[string]func(recordsCommitter) recordsHandler),
			topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
			partitionPool:      pool.New().WithContext(ctx),
		}

		// Run event loop in background
		runComplete := make(chan error, 1)
		go func() {
			runComplete <- loop.run(ctx)
		}()

		// 1. Assign partition
		ap := assignedPartition{
			topicPartition: topicPartition{topic: "test-topic", partition: 0},
			handler:        handler,
		}
		loop.assignedPartitions <- ap

		// Wait for assignment to be processed
		time.Sleep(50 * time.Millisecond)

		// 2. Send fetch
		fetch := kgo.FetchTopic{
			Topic: "test-topic",
			Partitions: []kgo.FetchPartition{
				{Partition: 0, Records: []*kgo.Record{{Topic: "test-topic"}}},
			},
		}
		loop.fetches <- fetch

		// Wait for handler to be called
		select {
		case <-handlerCalled:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("handler was not called")
		}

		// 3. Revoke partition
		loop.revokedPartitions <- ap.topicPartition

		// Wait for revocation to be processed
		time.Sleep(50 * time.Millisecond)

		// 4. Cancel context
		cancel()

		// Wait for run to complete
		select {
		case err := <-runComplete:
			// run() returns the result of shutdown(), which is nil when shutdown succeeds
			require.NoError(t, err)
		case <-time.After(1 * time.Second):
			t.Fatal("run did not complete")
		}

		// Verify handler was called
		calls := tracker.getCalls()
		assert.Contains(t, calls, "Handle", "handler should have been called")
	})
}

func TestEventLoop_Run_MissingPartitionWarnings(t *testing.T) {
	t.Run("will log warnings for operations on unknown partitions", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		capture := &captureHandler{Handler: slog.Default().Handler()}

		loop := eventLoop{
			log:                makeTestLogger(capture),
			fetches:            make(chan kgo.FetchTopic, 3),
			assignedPartitions: make(chan assignedPartition, 1),
			lostPartitions:     make(chan topicPartition, 1),
			revokedPartitions:  make(chan topicPartition, 1),
			topicHandlers:      make(map[string]func(recordsCommitter) recordsHandler),
			topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
			partitionPool:      pool.New().WithContext(ctx),
		}

		// Run event loop in background
		runComplete := make(chan error, 1)
		go func() {
			runComplete <- loop.run(ctx)
		}()

		tp := topicPartition{topic: "unknown-topic", partition: 99}

		// 1. Fetch for unknown partition
		loop.fetches <- kgo.FetchTopic{
			Topic: "unknown-topic",
			Partitions: []kgo.FetchPartition{
				{Partition: 99, Records: []*kgo.Record{{Topic: "unknown-topic"}}},
			},
		}

		time.Sleep(50 * time.Millisecond)

		// 2. Revoke unknown partition
		loop.revokedPartitions <- tp

		time.Sleep(50 * time.Millisecond)

		// 3. Lose unknown partition
		loop.lostPartitions <- tp

		time.Sleep(50 * time.Millisecond)

		// Cancel and wait for completion
		cancel()
		select {
		case <-runComplete:
			// Success
		case <-time.After(1 * time.Second):
			t.Fatal("run did not complete")
		}

		// Verify warnings were logged
		records := capture.getRecords()

		var warningMessages []string
		for _, record := range records {
			warningMessages = append(warningMessages, record.Message)
		}

		assert.Contains(t, warningMessages, "topic partition not found for fetched records")
		assert.Contains(t, warningMessages, "topic partition not found for revoked partition")
		assert.Contains(t, warningMessages, "topic partition not found for lost partition")
	})
}

// Edge case: processRecords function tests

func TestProcessRecords(t *testing.T) {
	t.Run("will process records until channel is closed", func(t *testing.T) {
		ctx := context.Background()
		recordsCh := make(chan []*kgo.Record, 2)

		processedCount := 0
		handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
			processedCount++
			return nil
		})

		// Send two batches then close
		recordsCh <- []*kgo.Record{{Topic: "test"}}
		recordsCh <- []*kgo.Record{{Topic: "test"}}
		close(recordsCh)

		processFunc := processRecords(recordsCh, handler)
		err := processFunc(ctx)

		require.NoError(t, err)
		require.Equal(t, 2, processedCount)
	})

	t.Run("will return error if handler fails", func(t *testing.T) {
		ctx := context.Background()
		recordsCh := make(chan []*kgo.Record, 1)

		handlerErr := errors.New("handler error")
		handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
			return handlerErr
		})

		recordsCh <- []*kgo.Record{{Topic: "test"}}

		processFunc := processRecords(recordsCh, handler)
		err := processFunc(ctx)

		require.ErrorIs(t, err, handlerErr)
	})

	t.Run("will return ctx.Err() when context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		recordsCh := make(chan []*kgo.Record)
		handler := recordsHandlerFunc(func(ctx context.Context, records []*kgo.Record) error {
			return nil
		})

		processFunc := processRecords(recordsCh, handler)
		err := processFunc(ctx)

		require.ErrorIs(t, err, context.Canceled)
	})
}
