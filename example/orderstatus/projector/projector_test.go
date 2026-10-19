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
	StatusTopic:     "orders.status",
	DeadLetterTopic: "orders.dlq",
}

type applied struct {
	event  store.Event
	status store.Status
}

type memoryStore struct {
	mu        sync.Mutex
	events    []store.Event
	applied   []applied
	applyErrs []error
}

func (s *memoryStore) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.events, func(e store.Event) bool {
		return e.Partition == partition && e.Offset == offset
	}), nil
}

func (s *memoryStore) Events(ctx context.Context, orderID string) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if len(s.applyErrs) > 0 {
		err := s.applyErrs[0]
		s.applyErrs = s.applyErrs[1:]
		return err
	}
	s.events = append(s.events, e)
	s.applied = append(s.applied, applied{event: e, status: status})
	return nil
}

type failingStore struct {
	memoryStore
	appliedErr error
	eventsErr  error
}

func (s *failingStore) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	if s.appliedErr != nil {
		return false, s.appliedErr
	}
	return s.memoryStore.Applied(ctx, partition, offset)
}

func (s *failingStore) Events(ctx context.Context, orderID string) ([]store.Event, error) {
	if s.eventsErr != nil {
		return nil, s.eventsErr
	}
	return s.memoryStore.Events(ctx, orderID)
}

type producer struct {
	mu      sync.Mutex
	records []kafka.Record
	err     error
}

func (p *producer) Produce(ctx context.Context, records ...kafka.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.records = append(p.records, records...)
	return nil
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

func TestProcess_ProducesEachChangeThenStoresTheEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, dlq, settings)

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
		p := projector.New(s, status, &producer{}, settings)

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
		p := projector.New(s, status, &producer{}, settings)

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
		p := projector.New(s, status, dlq, settings)

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
		p := projector.New(s, &producer{}, &producer{}, settings)

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
				p := projector.New(s, status, dlq, settings)

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
		p := projector.New(&memoryStore{}, &producer{}, &producer{err: errDown}, settings)

		err := p.Process(t.Context(), message(20, `placed`))

		require.ErrorIs(t, err, errDown)
	})
}

func TestProcess_AStoreThatFailsAfterTheChangeWasProduced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errDown := errors.New("postgres down")
		s := &memoryStore{applyErrs: []error{errDown}}
		status := &producer{}
		p := projector.New(s, status, &producer{}, settings)
		m := eventMessage(10, projector.Placed, 0)

		err := p.Process(t.Context(), m)
		require.ErrorIs(t, err, errDown)
		require.Len(t, status.records, 1)
		require.Empty(t, s.applied)

		require.NoError(t, p.Process(t.Context(), m))

		placed := statusChange{OrderID: "order-1", Status: projector.Placed, Previous: "", At: start}
		require.Equal(t, []statusChange{placed, placed}, changes(t, status.records))
		require.Equal(t, []applied{
			{event: storedEvent(10, projector.Placed, 0), status: store.Status{Status: projector.Placed, At: start}},
		}, s.applied)
	})
}

func TestProcess_AHistoryTheFoldRefusesIsTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		dlq := &producer{}
		p := projector.New(s, status, dlq, settings)
		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		status.records = nil

		err := p.Process(t.Context(), eventMessage(11, projector.Shipped, 1))

		var transition *projector.TransitionError
		require.ErrorAs(t, err, &transition)
		require.Empty(t, status.records)
		require.Empty(t, dlq.records)
		require.Len(t, s.applied, 1)
	})
}

func TestProcess_AChangeThatCannotBeProducedIsTheErrorAndNothingIsStored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errDown := errors.New("cluster down")
		s := &memoryStore{}
		p := projector.New(s, &producer{err: errDown}, &producer{}, settings)

		err := p.Process(t.Context(), eventMessage(10, projector.Placed, 0))

		require.ErrorIs(t, err, errDown)
		require.Empty(t, s.applied)
	})
}

func TestProcess_AStoreThatCannotBeReadIsTheError(t *testing.T) {
	errDown := errors.New("postgres down")
	testCases := []struct {
		name  string
		store *failingStore
	}{
		{name: "finding whether it was applied", store: &failingStore{appliedErr: errDown}},
		{name: "reading the order's events", store: &failingStore{eventsErr: errDown}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				status := &producer{}
				p := projector.New(tc.store, status, &producer{}, settings)

				err := p.Process(t.Context(), eventMessage(10, projector.Placed, 0))

				require.ErrorIs(t, err, errDown)
				require.Empty(t, status.records)
				require.Empty(t, tc.store.applied)
			})
		})
	}
}
