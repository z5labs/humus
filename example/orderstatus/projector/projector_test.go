// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package projector_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/queue/kafka"
)

var (
	_ projector.Store    = (*store.Store)(nil)
	_ projector.Producer = (*kafka.Producer)(nil)
)

var settings = projector.Settings{
	EventsTopic:     "orders.events",
	StatusTopic:     "orders.status",
	RetryTopic:      "orders.retry",
	DeadLetterTopic: "orders.dlq",
	Attempts:        3,
	MinDelay:        time.Second,
	MaxDelay:        5 * time.Second,
}

type applied struct {
	event  store.Event
	status store.Status
}

type memoryStore struct {
	mu      sync.Mutex
	events  []store.Event
	applied []applied

	appliedErrs []error
	eventsErrs  []error
	applyErrs   []error

	appliedErr error
	eventsErr  error

	appliedCalls int
	eventsCalls  int
	applyCalls   int
}

func next(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (s *memoryStore) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appliedCalls++
	if err := firstErr(s.appliedErr, next(&s.appliedErrs)); err != nil {
		return false, err
	}
	return slices.ContainsFunc(s.events, func(e store.Event) bool {
		return e.Partition == partition && e.Offset == offset
	}), nil
}

func (s *memoryStore) Events(ctx context.Context, orderID string) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventsCalls++
	if err := firstErr(s.eventsErr, next(&s.eventsErrs)); err != nil {
		return nil, err
	}
	var events []store.Event
	for _, e := range s.events {
		if e.OrderID == orderID {
			events = append(events, e)
		}
	}
	return events, nil
}

func (s *memoryStore) Apply(ctx context.Context, e store.Event, status store.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	if err := next(&s.applyErrs); err != nil {
		return err
	}
	s.events = append(s.events, e)
	s.applied = append(s.applied, applied{event: e, status: status})
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type producer struct {
	mu      sync.Mutex
	records []kafka.Record
	times   []time.Time
	err     error
	block   bool
}

func (p *producer) Produce(ctx context.Context, records ...kafka.Record) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.records = append(p.records, records...)
	for range records {
		p.times = append(p.times, time.Now())
	}
	return nil
}

func (p *producer) last(t *testing.T) (kafka.Record, time.Time) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotEmpty(t, p.records)
	return p.records[len(p.records)-1], p.times[len(p.times)-1]
}

func message(offset int64, value string) kafka.Message {
	return kafka.Message{
		Topic:     "orders.events",
		Partition: 4,
		Offset:    offset,
		Key:       []byte("order-1"),
		Value:     []byte(value),
		Headers:   []kafka.Header{{Key: "source", Value: []byte("checkout")}},
		Timestamp: start,
	}
}

func eventMessage(offset int64, typ string, minute int) kafka.Message {
	at := start.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	return message(offset, fmt.Sprintf(`{"type":%q,"at":%q}`, typ, at))
}

func redeliver(r kafka.Record, offset int64, at time.Time) kafka.Message {
	return kafka.Message{
		Topic:     r.Topic,
		Partition: 7,
		Offset:    offset,
		Key:       r.Key,
		Value:     r.Value,
		Headers:   r.Headers,
		Timestamp: at,
	}
}

func retryMessage(m kafka.Message, attempt int, at time.Time) kafka.Message {
	headers := slices.Clone(m.Headers)
	headers = append(headers,
		kafka.Header{Key: "retry.attempt", Value: []byte(strconv.Itoa(attempt))},
		kafka.Header{Key: "retry.reason", Value: []byte("invalid-transition")},
		kafka.Header{Key: "retry.error", Value: []byte("refused")},
		kafka.Header{Key: "retry.partition", Value: []byte(strconv.FormatInt(int64(m.Partition), 10))},
		kafka.Header{Key: "retry.offset", Value: []byte(strconv.FormatInt(m.Offset, 10))},
	)
	return kafka.Message{
		Topic:     "orders.retry",
		Partition: 7,
		Offset:    900,
		Key:       m.Key,
		Value:     m.Value,
		Headers:   headers,
		Timestamp: at,
	}
}

func storedEvent(offset int64, typ string, minute int) store.Event {
	return store.Event{
		Partition: 4,
		Offset:    offset,
		OrderID:   "order-1",
		Type:      typ,
		At:        start.Add(time.Duration(minute) * time.Minute),
	}
}

type statusChange struct {
	OrderID  string    `json:"order_id"`
	Status   string    `json:"status"`
	Previous string    `json:"previous"`
	At       time.Time `json:"at"`
}

func changes(t *testing.T, records []kafka.Record) []statusChange {
	t.Helper()
	var cs []statusChange
	for _, r := range records {
		require.Equal(t, "orders.status", r.Topic)
		require.Equal(t, "order-1", string(r.Key))
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(r.Value, &fields))
		require.ElementsMatch(t, []string{"order_id", "status", "previous", "at"}, slices.Collect(maps.Keys(fields)))
		var c statusChange
		require.NoError(t, json.Unmarshal(r.Value, &c))
		cs = append(cs, c)
	}
	return cs
}

func headerMap(t *testing.T, headers []kafka.Header) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, h := range headers {
		_, dup := m[h.Key]
		require.False(t, dup, "header %s repeats", h.Key)
		m[h.Key] = string(h.Value)
	}
	return m
}

func TestProcess_ProducesEachChangeThenStoresTheEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, &producer{}, dlq, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Paid, 1)))
		require.NoError(t, p.Process(t.Context(), eventMessage(12, projector.Shipped, 2)))

		require.Equal(t, []statusChange{
			{OrderID: "order-1", Status: projector.Placed, Previous: "", At: start},
			{OrderID: "order-1", Status: projector.Paid, Previous: projector.Placed, At: start.Add(time.Minute)},
			{OrderID: "order-1", Status: projector.Shipped, Previous: projector.Paid, At: start.Add(2 * time.Minute)},
		}, changes(t, status.records))
		require.Equal(t, []applied{
			{event: storedEvent(10, projector.Placed, 0), status: store.Status{Status: projector.Placed, At: start}},
			{event: storedEvent(11, projector.Paid, 1), status: store.Status{Status: projector.Paid, At: start.Add(time.Minute)}},
			{event: storedEvent(12, projector.Shipped, 2), status: store.Status{Status: projector.Shipped, At: start.Add(2 * time.Minute)}},
		}, s.applied)
		require.Empty(t, dlq.records)
	})
}

func TestProcess_ALateEventProducesItsChangeAndEveryLaterOneAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		p := projector.New(s, status, &producer{}, &producer{}, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Cancelled, 2)))
		status.records = nil

		require.NoError(t, p.Process(t.Context(), eventMessage(12, projector.Paid, 1)))

		require.Equal(t, []statusChange{
			{OrderID: "order-1", Status: projector.Paid, Previous: projector.Placed, At: start.Add(time.Minute)},
			{OrderID: "order-1", Status: projector.Cancelled, Previous: projector.Paid, At: start.Add(2 * time.Minute)},
		}, changes(t, status.records))
		require.Equal(t, applied{
			event:  storedEvent(12, projector.Paid, 1),
			status: store.Status{Status: projector.Cancelled, At: start.Add(2 * time.Minute)},
		}, s.applied[2])
	})
}

func TestProcess_AnEventThatRepeatsTheStatusIsStoredAndProducesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		p := projector.New(s, status, &producer{}, &producer{}, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		status.records = nil

		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Placed, 1)))

		require.Empty(t, status.records)
		require.Len(t, s.applied, 2)
		require.Equal(t, store.Status{Status: projector.Placed, At: start}, s.applied[1].status)
	})
}

func TestProcess_ARedeliveredEventProducesAndStoresNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, &producer{}, dlq, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Paid, 1)))
		status.records = nil

		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Paid, 1)))

		require.Empty(t, status.records)
		require.Empty(t, dlq.records)
		require.Len(t, s.applied, 2)
	})
}

func TestProcess_AnEventAnotherWriterAppliedFirstIsNotAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{applyErrs: []error{fmt.Errorf("storing: %w", store.ErrApplied)}}
		p := projector.New(s, &producer{}, &producer{}, &producer{}, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
	})
}

func TestProcess_DeadLetters(t *testing.T) {
	at := start.Format(time.RFC3339)
	testCases := []struct {
		name   string
		msg    kafka.Message
		reason string
	}{
		{name: "not JSON", msg: message(20, `placed`), reason: "malformed"},
		{name: "not an object", msg: message(20, `["placed"]`), reason: "malformed"},
		{
			name: "no key",
			msg: func() kafka.Message {
				m := eventMessage(20, projector.Placed, 0)
				m.Key = nil
				return m
			}(),
			reason: "malformed",
		},
		{name: "no type", msg: message(20, fmt.Sprintf(`{"at":%q}`, at)), reason: "malformed"},
		{name: "no at", msg: message(20, `{"type":"placed"}`), reason: "malformed"},
		{name: "an at that isn't a time", msg: message(20, `{"type":"placed","at":"yesterday"}`), reason: "malformed"},
		{name: "an unknown type", msg: message(20, fmt.Sprintf(`{"type":"returned","at":%q}`, at)), reason: "unknown-type"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &memoryStore{}
				status := &producer{}
				dlq := &producer{}
				p := projector.New(s, status, &producer{}, dlq, settings)

				require.NoError(t, p.Process(t.Context(), tc.msg))

				require.Len(t, dlq.records, 1)
				r := dlq.records[0]
				require.Equal(t, "orders.dlq", r.Topic)
				require.Equal(t, tc.msg.Key, r.Key)
				require.Equal(t, tc.msg.Value, r.Value)

				require.Len(t, r.Headers, 6)
				require.Equal(t, kafka.Header{Key: "source", Value: []byte("checkout")}, r.Headers[0])
				headers := map[string]string{}
				for _, h := range r.Headers[1:] {
					headers[h.Key] = string(h.Value)
				}
				require.NotEmpty(t, headers["dlq.error"])
				delete(headers, "dlq.error")
				require.Equal(t, map[string]string{
					"dlq.reason":    tc.reason,
					"dlq.topic":     "orders.events",
					"dlq.partition": "4",
					"dlq.offset":    "20",
				}, headers)

				require.Empty(t, status.records)
				require.Empty(t, s.applied)
			})
		})
	}
}

func TestProcess_ADeadLetterThatCannotBeProducedIsTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errDown := errors.New("cluster down")
		p := projector.New(&memoryStore{}, &producer{}, &producer{}, &producer{err: errDown}, settings)

		err := p.Process(t.Context(), message(20, `placed`))

		require.ErrorIs(t, err, errDown)
	})
}

func TestProcess_RetriesThenDeadLetters(t *testing.T) {
	errDown := errors.New("down")
	testCases := []struct {
		name   string
		store  func() *memoryStore
		status func() *producer
		reason string
	}{
		{
			name: "a refused history",
			store: func() *memoryStore {
				return &memoryStore{events: []store.Event{storedEvent(10, projector.Placed, 0)}}
			},
			status: func() *producer { return &producer{} },
			reason: "invalid-transition",
		},
		{
			name:   "a store that keeps failing",
			store:  func() *memoryStore { return &memoryStore{eventsErr: errDown} },
			status: func() *producer { return &producer{} },
			reason: "unavailable",
		},
		{
			name:   "a change that cannot be produced",
			store:  func() *memoryStore { return &memoryStore{} },
			status: func() *producer { return &producer{err: errDown} },
			reason: "unavailable",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store()
				status := tc.status()
				retries := &producer{}
				dlq := &producer{}
				p := projector.New(s, status, retries, dlq, settings)
				m := eventMessage(11, projector.Shipped, 1)
				if tc.reason == "unavailable" {
					m = eventMessage(11, projector.Placed, 1)
				}

				require.NoError(t, p.Process(t.Context(), m))

				r, at := retries.last(t)
				require.Equal(t, "orders.retry", r.Topic)
				require.Equal(t, m.Key, r.Key)
				require.Equal(t, m.Value, r.Value)
				require.Equal(t, kafka.Header{Key: "source", Value: []byte("checkout")}, r.Headers[0])
				headers := headerMap(t, r.Headers)
				require.NotEmpty(t, headers["retry.error"])
				delete(headers, "retry.error")
				require.Equal(t, map[string]string{
					"source":          "checkout",
					"retry.attempt":   "1",
					"retry.reason":    tc.reason,
					"retry.partition": "4",
					"retry.offset":    "11",
				}, headers)

				require.NoError(t, p.Process(t.Context(), redeliver(r, 100, at)))

				require.Len(t, retries.records, 2)
				r, at = retries.last(t)
				headers = headerMap(t, r.Headers)
				delete(headers, "retry.error")
				require.Equal(t, map[string]string{
					"source":          "checkout",
					"retry.attempt":   "2",
					"retry.reason":    tc.reason,
					"retry.partition": "4",
					"retry.offset":    "11",
				}, headers)
				require.Empty(t, dlq.records)

				require.NoError(t, p.Process(t.Context(), redeliver(r, 101, at)))

				require.Len(t, retries.records, 2)
				require.Len(t, dlq.records, 1)
				d := dlq.records[0]
				require.Equal(t, "orders.dlq", d.Topic)
				require.Equal(t, m.Key, d.Key)
				require.Equal(t, m.Value, d.Value)
				headers = headerMap(t, d.Headers)
				require.NotEmpty(t, headers["dlq.error"])
				require.Equal(t, tc.reason, headers["dlq.reason"])
				require.Equal(t, "3", headers["dlq.attempts"])
				require.Equal(t, "orders.events", headers["dlq.topic"])
				require.Equal(t, "4", headers["dlq.partition"])
				require.Equal(t, "11", headers["dlq.offset"])
				require.Empty(t, s.applied)
			})
		})
	}
}

func TestProcess_ARetriedRecordWaitsItsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{events: []store.Event{storedEvent(10, projector.Placed, 0)}}
		retries := &producer{}
		dlq := &producer{}
		ten := settings
		ten.Attempts = 10
		p := projector.New(s, &producer{}, retries, dlq, ten)

		m := eventMessage(11, projector.Shipped, 1)
		var waits []time.Duration
		for i := range 10 {
			before := time.Now()
			require.NoError(t, p.Process(t.Context(), m))
			waits = append(waits, time.Since(before))
			if i < 9 {
				r, at := retries.last(t)
				m = redeliver(r, int64(100+i), at)
			}
		}

		require.Equal(t, []time.Duration{
			0,
			time.Second,
			2 * time.Second,
			4 * time.Second,
			5 * time.Second,
			5 * time.Second,
			5 * time.Second,
			5 * time.Second,
			5 * time.Second,
			5 * time.Second,
		}, waits)
		require.Len(t, retries.records, 9)
		require.Len(t, dlq.records, 1)
		require.Equal(t, "10", headerMap(t, dlq.records[0].Headers)["dlq.attempts"])
	})
}

func TestProcess_ARetriedRecordWaitsFromItsTimestamp(t *testing.T) {
	testCases := []struct {
		name string
		age  time.Duration
		wait time.Duration
	}{
		{name: "produced just now", age: 0, wait: 4 * time.Second},
		{name: "produced a second ago", age: time.Second, wait: 3 * time.Second},
		{name: "produced longer ago than its backoff", age: 10 * time.Second, wait: 0},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &memoryStore{events: []store.Event{storedEvent(10, projector.Placed, 0)}}
				ten := settings
				ten.Attempts = 10
				p := projector.New(s, &producer{}, &producer{}, &producer{}, ten)
				m := retryMessage(eventMessage(11, projector.Paid, 1), 3, time.Now().Add(-tc.age))

				before := time.Now()
				require.NoError(t, p.Process(t.Context(), m))

				require.Equal(t, tc.wait, time.Since(before))
				require.Len(t, s.applied, 1)
			})
		})
	}
}

func TestProcess_AWaitHonoursItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		retries := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, retries, dlq, settings)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(500*time.Millisecond, cancel)

		before := time.Now()
		err := p.Process(ctx, retryMessage(eventMessage(10, projector.Placed, 0), 2, time.Now()))

		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 500*time.Millisecond, time.Since(before))
		require.Empty(t, status.records)
		require.Empty(t, retries.records)
		require.Empty(t, dlq.records)
		require.Zero(t, s.appliedCalls)
	})
}

func TestProcess_ARefusedEventFoldsOnceItsRetryComesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		retries := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, retries, dlq, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Shipped, 2)))
		require.NoError(t, p.Process(t.Context(), eventMessage(12, projector.Paid, 1)))
		r, at := retries.last(t)
		require.NoError(t, p.Process(t.Context(), redeliver(r, 100, at)))

		require.Equal(t, []statusChange{
			{OrderID: "order-1", Status: projector.Placed, Previous: "", At: start},
			{OrderID: "order-1", Status: projector.Paid, Previous: projector.Placed, At: start.Add(time.Minute)},
			{OrderID: "order-1", Status: projector.Shipped, Previous: projector.Paid, At: start.Add(2 * time.Minute)},
		}, changes(t, status.records))
		require.Len(t, retries.records, 1)
		require.Empty(t, dlq.records)
		require.Equal(t, applied{
			event:  storedEvent(11, projector.Shipped, 2),
			status: store.Status{Status: projector.Shipped, At: start.Add(2 * time.Minute)},
		}, s.applied[len(s.applied)-1])
	})
}

func TestProcess_ARetriedRecordAppliedAlreadyIsSkippedByItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{events: []store.Event{
			storedEvent(10, projector.Placed, 0),
			storedEvent(11, projector.Paid, 1),
		}}
		status := &producer{}
		retries := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, retries, dlq, settings)

		require.NoError(t, p.Process(t.Context(), retryMessage(eventMessage(11, projector.Paid, 1), 1, time.Now())))

		require.Empty(t, status.records)
		require.Empty(t, retries.records)
		require.Empty(t, dlq.records)
		require.Empty(t, s.applied)
		require.Zero(t, s.eventsCalls)
	})
}

func TestProcess_TheStoreIsRetriedInProcess(t *testing.T) {
	errDown := errors.New("postgres down")
	failing := func(n int) []error {
		errs := make([]error, n)
		for i := range errs {
			errs[i] = errDown
		}
		return errs
	}
	testCases := []struct {
		name  string
		store func(failures int) *memoryStore
		calls func(s *memoryStore) int
	}{
		{
			name:  "finding whether it was applied",
			store: func(n int) *memoryStore { return &memoryStore{appliedErrs: failing(n)} },
			calls: func(s *memoryStore) int { return s.appliedCalls },
		},
		{
			name:  "reading the order's events",
			store: func(n int) *memoryStore { return &memoryStore{eventsErrs: failing(n)} },
			calls: func(s *memoryStore) int { return s.eventsCalls },
		},
		{
			name:  "storing the event",
			store: func(n int) *memoryStore { return &memoryStore{applyErrs: failing(n)} },
			calls: func(s *memoryStore) int { return s.applyCalls },
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name+" succeeds on its last attempt", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store(4)
				status := &producer{}
				retries := &producer{}
				p := projector.New(s, status, retries, &producer{}, settings)

				require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))

				require.Equal(t, 5, tc.calls(s))
				require.Empty(t, retries.records)
				require.Len(t, status.records, 1)
				require.Equal(t, []applied{
					{event: storedEvent(10, projector.Placed, 0), status: store.Status{Status: projector.Placed, At: start}},
				}, s.applied)
			})
		})
		t.Run(tc.name+" fails on every attempt", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store(5)
				retries := &producer{}
				p := projector.New(s, &producer{}, retries, &producer{}, settings)

				require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))

				require.Equal(t, 5, tc.calls(s))
				require.Empty(t, s.applied)
				r, _ := retries.last(t)
				headers := headerMap(t, r.Headers)
				require.Equal(t, "unavailable", headers["retry.reason"])
				require.Contains(t, headers["retry.error"], errDown.Error())
			})
		})
	}
}

func TestProcess_WorkThatOutlastsItsDeadlineIsRetried(t *testing.T) {
	testCases := []struct {
		name    string
		msg     func() kafka.Message
		elapsed time.Duration
		attempt string
	}{
		{
			name:    "from the events topic",
			msg:     func() kafka.Message { return eventMessage(10, projector.Placed, 0) },
			elapsed: 15 * time.Second,
			attempt: "1",
		},
		{
			name:    "counted after a retried record's wait",
			msg:     func() kafka.Message { return retryMessage(eventMessage(10, projector.Placed, 0), 1, time.Now()) },
			elapsed: 16 * time.Second,
			attempt: "2",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &memoryStore{}
				retries := &producer{}
				p := projector.New(s, &producer{block: true}, retries, &producer{}, settings)

				before := time.Now()
				require.NoError(t, p.Process(t.Context(), tc.msg()))

				require.Equal(t, tc.elapsed, time.Since(before))
				require.Empty(t, s.applied)
				r, _ := retries.last(t)
				headers := headerMap(t, r.Headers)
				require.Equal(t, tc.attempt, headers["retry.attempt"])
				require.Equal(t, "unavailable", headers["retry.reason"])
				require.Contains(t, headers["retry.error"], context.DeadlineExceeded.Error())
			})
		})
	}
}

func TestProcess_ACancelledContextIsTheErrorAndNotARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		retries := &producer{}
		dlq := &producer{}
		p := projector.New(s, &producer{block: true}, retries, dlq, settings)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)

		err := p.Process(ctx, eventMessage(10, projector.Placed, 0))

		require.ErrorIs(t, err, context.Canceled)
		require.Empty(t, retries.records)
		require.Empty(t, dlq.records)
		require.Empty(t, s.applied)
	})
}

func TestProcess_ARecordThatCannotGoAnywhereIsTheError(t *testing.T) {
	errDown := errors.New("cluster down")
	testCases := []struct {
		name    string
		msg     func() kafka.Message
		retries *producer
		dlq     *producer
	}{
		{
			name:    "a retry that cannot be produced",
			msg:     func() kafka.Message { return eventMessage(11, projector.Shipped, 1) },
			retries: &producer{err: errDown},
			dlq:     &producer{},
		},
		{
			name:    "a dead letter after the last attempt that cannot be produced",
			msg:     func() kafka.Message { return retryMessage(eventMessage(11, projector.Shipped, 1), 2, time.Now()) },
			retries: &producer{},
			dlq:     &producer{err: errDown},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &memoryStore{events: []store.Event{storedEvent(10, projector.Placed, 0)}}
				p := projector.New(s, &producer{}, tc.retries, tc.dlq, settings)

				err := p.Process(t.Context(), tc.msg())

				require.ErrorIs(t, err, errDown)
				require.ErrorContains(t, err, "orders.events, partition 4, offset 11")
				require.Empty(t, tc.retries.records)
				require.Empty(t, tc.dlq.records)
			})
		})
	}
}

func TestProcess_RetryHeadersOnARecordFromTheEventsTopicAreIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{events: []store.Event{storedEvent(10, projector.Placed, 0)}}
		retries := &producer{}
		p := projector.New(s, &producer{}, retries, &producer{}, settings)
		m := eventMessage(11, projector.Shipped, 1)
		m.Headers = append(m.Headers,
			kafka.Header{Key: "retry.attempt", Value: []byte("9")},
			kafka.Header{Key: "retry.offset", Value: []byte("10")},
		)

		before := time.Now()
		require.NoError(t, p.Process(t.Context(), m))

		require.Zero(t, time.Since(before))
		r, _ := retries.last(t)
		headers := headerMap(t, r.Headers)
		require.Equal(t, "1", headers["retry.attempt"])
		require.Equal(t, "4", headers["retry.partition"])
		require.Equal(t, "11", headers["retry.offset"])
	})
}

func TestProcess_ARetriedRecordWithBadHeadersIsADeadLetter(t *testing.T) {
	without := func(key string) func([]kafka.Header) []kafka.Header {
		return func(hs []kafka.Header) []kafka.Header {
			return slices.DeleteFunc(hs, func(h kafka.Header) bool { return h.Key == key })
		}
	}
	set := func(key, value string) func([]kafka.Header) []kafka.Header {
		return func(hs []kafka.Header) []kafka.Header {
			for i := range hs {
				if hs[i].Key == key {
					hs[i].Value = []byte(value)
				}
			}
			return hs
		}
	}
	testCases := []struct {
		name   string
		change func([]kafka.Header) []kafka.Header
	}{
		{name: "no attempt", change: without("retry.attempt")},
		{name: "an attempt of 0", change: set("retry.attempt", "0")},
		{name: "an attempt that isn't a number", change: set("retry.attempt", "two")},
		{name: "no partition", change: without("retry.partition")},
		{name: "a partition too large", change: set("retry.partition", "4294967296")},
		{name: "no offset", change: without("retry.offset")},
		{name: "a negative offset", change: set("retry.offset", "-1")},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &memoryStore{}
				retries := &producer{}
				dlq := &producer{}
				p := projector.New(s, &producer{}, retries, dlq, settings)
				m := retryMessage(eventMessage(10, projector.Placed, 0), 1, time.Now())
				m.Headers = tc.change(m.Headers)

				require.NoError(t, p.Process(t.Context(), m))

				require.Empty(t, retries.records)
				require.Empty(t, s.applied)
				require.Len(t, dlq.records, 1)
				headers := map[string]string{}
				for _, h := range dlq.records[0].Headers {
					headers[h.Key] = string(h.Value)
				}
				require.Equal(t, "malformed", headers["dlq.reason"])
				require.Equal(t, "orders.retry", headers["dlq.topic"])
				require.Equal(t, "7", headers["dlq.partition"])
				require.Equal(t, "900", headers["dlq.offset"])
				require.NotContains(t, headers, "dlq.attempts")
			})
		})
	}
}
