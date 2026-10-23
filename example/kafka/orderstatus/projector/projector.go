// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package projector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/z5labs/humus/example/kafka/orderstatus/store"
	"github.com/z5labs/humus/queue/kafka"
)

const scope = "github.com/z5labs/humus/example/kafka/orderstatus"

const reasonKey = attribute.Key("orderstatus.reason")

const (
	reasonMalformed         = "malformed"
	reasonUnknownType       = "unknown-type"
	reasonInvalidTransition = "invalid-transition"
	reasonUnavailable       = "unavailable"
)

const (
	headerAttempt   = "retry.attempt"
	headerReason    = "retry.reason"
	headerError     = "retry.error"
	headerPartition = "retry.partition"
	headerOffset    = "retry.offset"
)

const (
	workDeadline        = 15 * time.Second
	storeAttempts       = 5
	storeMinDelay       = 100 * time.Millisecond
	storeMaxDelay       = 2 * time.Second
	storeAttemptTimeout = 2 * time.Second
)

type Store interface {
	Applied(ctx context.Context, partition int32, offset int64) (bool, error)
	Events(ctx context.Context, orderID string) ([]store.Event, error)
	Apply(ctx context.Context, e store.Event, status store.Status) error
}

type Producer interface {
	Produce(ctx context.Context, records ...kafka.Record) error
}

type Settings struct {
	EventsTopic     string
	StatusTopic     string
	RetryTopic      string
	DeadLetterTopic string
	Attempts        int
	MinDelay        time.Duration
	MaxDelay        time.Duration
}

type Projector struct {
	store       Store
	retrier     *retry.Retrier
	status      Producer
	retries     Producer
	deadLetters Producer
	settings    Settings
	retried     metric.Int64Counter
	dropped     metric.Int64Counter
}

func New(s Store, status, retries, deadLetters Producer, settings Settings, meters metric.MeterProvider) (*Projector, error) {
	schedule, err := retry.Exponential(storeMinDelay, storeMaxDelay)
	if err != nil {
		return nil, err
	}
	r, err := retry.New(storeAttempts, schedule, classifyStore, nil)
	if err != nil {
		return nil, err
	}
	meter := meters.Meter(scope)
	retried, err := meter.Int64Counter("orderstatus.retries",
		metric.WithDescription("Records produced to the retry topic, by the reason their attempt failed."),
		metric.WithUnit("{record}"))
	if err != nil {
		return nil, err
	}
	dropped, err := meter.Int64Counter("orderstatus.dead_letters",
		metric.WithDescription("Records produced to the dead-letter topic, by the reason they were given up on."),
		metric.WithUnit("{record}"))
	if err != nil {
		return nil, err
	}
	return &Projector{
		store:       s,
		retrier:     r,
		status:      status,
		retries:     retries,
		deadLetters: deadLetters,
		settings:    settings,
		retried:     retried,
		dropped:     dropped,
	}, nil
}

func classifyStore(ctx context.Context, err error) (retry.Outcome, time.Duration) {
	switch {
	case err == nil:
		return retry.Success, 0
	case errors.Is(err, store.ErrApplied):
		return retry.Stop, 0
	default:
		return retry.Retry, 0
	}
}

type place struct {
	topic     string
	partition int32
	offset    int64
}

func (p *Projector) Process(ctx context.Context, m kafka.Message) error {
	attempt, from, err := p.origin(m)
	if err != nil {
		return p.deadLetter(ctx, m, from, reasonMalformed, err, 0)
	}

	e, reason, err := decode(m, from)
	if err != nil {
		return p.deadLetter(ctx, m, from, reason, err, 0)
	}

	err = p.wait(ctx, m, attempt)
	if err != nil {
		return err
	}

	work, cancel := context.WithTimeout(ctx, workDeadline)
	defer cancel()
	reason, err = p.project(work, m, e)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("projecting %s, partition %d, offset %d: %w", from.topic, from.partition, from.offset, context.Cause(ctx))
	}
	return p.retry(ctx, m, from, attempt, reason, err)
}

func (p *Projector) origin(m kafka.Message) (int, place, error) {
	own := place{topic: m.Topic, partition: m.Partition, offset: m.Offset}
	if m.Topic != p.settings.RetryTopic {
		return 0, own, nil
	}

	var errs []error
	attempt, err := header(m, headerAttempt, func(s string) (int, error) {
		n, err := strconv.Atoi(s)
		if err == nil && n < 1 {
			err = errors.New("it is less than 1")
		}
		return n, err
	})
	errs = append(errs, err)
	partition, err := header(m, headerPartition, func(s string) (int32, error) {
		n, err := strconv.ParseInt(s, 10, 32)
		if err == nil && n < 0 {
			err = errors.New("it is negative")
		}
		return int32(n), err
	})
	errs = append(errs, err)
	offset, err := header(m, headerOffset, func(s string) (int64, error) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil && n < 0 {
			err = errors.New("it is negative")
		}
		return n, err
	})
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return 0, own, err
	}
	return attempt, place{topic: p.settings.EventsTopic, partition: partition, offset: offset}, nil
}

func header[T any](m kafka.Message, key string, parse func(string) (T, error)) (T, error) {
	var zero T
	value, found := "", false
	for _, h := range m.Headers {
		if h.Key == key {
			value, found = string(h.Value), true
		}
	}
	if !found {
		return zero, fmt.Errorf("the record has no %s header", key)
	}
	v, err := parse(value)
	if err != nil {
		return zero, fmt.Errorf("the %s header %q is not valid: %w", key, value, err)
	}
	return v, nil
}

func (p *Projector) backoff(attempt int) time.Duration {
	d := p.settings.MinDelay
	for i := 1; i < attempt && d > 0 && d < p.settings.MaxDelay; i++ {
		d *= 2
	}
	return min(d, p.settings.MaxDelay)
}

func (p *Projector) wait(ctx context.Context, m kafka.Message, attempt int) error {
	if attempt == 0 {
		return nil
	}
	d := time.Until(m.Timestamp.Add(p.backoff(attempt)))
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting to retry %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, context.Cause(ctx))
	case <-t.C:
		return nil
	}
}

func (p *Projector) call(ctx context.Context, f func(context.Context) error) error {
	var err error
	for a := range p.retrier.Attempts(ctx) {
		err = attemptStore(ctx, f)
		a.Done(err)
	}
	return err
}

func attemptStore(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, storeAttemptTimeout)
	defer cancel()
	return f(ctx)
}

func (p *Projector) project(ctx context.Context, m kafka.Message, e store.Event) (string, error) {
	var applied bool
	err := p.call(ctx, func(ctx context.Context) error {
		var err error
		applied, err = p.store.Applied(ctx, e.Partition, e.Offset)
		return err
	})
	if err != nil {
		return reasonUnavailable, err
	}
	if applied {
		return "", nil
	}

	var history []store.Event
	err = p.call(ctx, func(ctx context.Context) error {
		var err error
		history, err = p.store.Events(ctx, e.OrderID)
		return err
	})
	if err != nil {
		return reasonUnavailable, err
	}
	status, changes, err := Fold(history, e)
	if err != nil {
		return reasonInvalidTransition, err
	}

	if len(changes) > 0 {
		records := make([]kafka.Record, 0, len(changes))
		for _, c := range changes {
			records = append(records, kafka.Record{
				Topic: p.settings.StatusTopic,
				Key:   m.Key,
				Value: encodeChange(e.OrderID, c),
			})
		}
		err := p.status.Produce(ctx, records...)
		if err != nil {
			return reasonUnavailable, fmt.Errorf("producing order %s's changes from partition %d, offset %d: %w", e.OrderID, e.Partition, e.Offset, err)
		}
	}

	err = p.call(ctx, func(ctx context.Context) error {
		return p.store.Apply(ctx, e, status)
	})
	if err != nil && !errors.Is(err, store.ErrApplied) {
		return reasonUnavailable, err
	}
	return "", nil
}

func (p *Projector) retry(ctx context.Context, m kafka.Message, from place, attempt int, reason string, cause error) error {
	failed := attempt + 1
	if failed >= p.settings.Attempts {
		return p.deadLetter(ctx, m, from, reason, cause, failed)
	}

	headers := make([]kafka.Header, 0, len(m.Headers)+5)
	for _, h := range m.Headers {
		if !strings.HasPrefix(h.Key, "retry.") {
			headers = append(headers, h)
		}
	}
	headers = append(headers,
		kafka.Header{Key: headerAttempt, Value: []byte(strconv.Itoa(failed))},
		kafka.Header{Key: headerReason, Value: []byte(reason)},
		kafka.Header{Key: headerError, Value: []byte(cause.Error())},
		kafka.Header{Key: headerPartition, Value: []byte(strconv.FormatInt(int64(from.partition), 10))},
		kafka.Header{Key: headerOffset, Value: []byte(strconv.FormatInt(from.offset, 10))},
	)
	err := p.retries.Produce(ctx, kafka.Record{
		Topic:   p.settings.RetryTopic,
		Key:     m.Key,
		Value:   m.Value,
		Headers: headers,
	})
	if err != nil {
		return fmt.Errorf("retrying %s, partition %d, offset %d as %s, attempt %d: %w", from.topic, from.partition, from.offset, reason, failed, err)
	}
	p.retried.Add(ctx, 1, metric.WithAttributes(reasonKey.String(reason)))
	return nil
}

type event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

func decode(m kafka.Message, from place) (store.Event, string, error) {
	if len(m.Key) == 0 {
		return store.Event{}, reasonMalformed, errors.New("the record has no key")
	}
	var v event
	if err := json.Unmarshal(m.Value, &v); err != nil {
		return store.Event{}, reasonMalformed, fmt.Errorf("the value is not an event: %w", err)
	}
	if v.Type == "" {
		return store.Event{}, reasonMalformed, errors.New("the event has no type")
	}
	if v.At.IsZero() {
		return store.Event{}, reasonMalformed, errors.New("the event has no at")
	}
	if _, ok := transitions[v.Type]; !ok {
		return store.Event{}, reasonUnknownType, fmt.Errorf("the event's type %q is not one this processor knows", v.Type)
	}
	return store.Event{
		Partition: from.partition,
		Offset:    from.offset,
		OrderID:   string(m.Key),
		Type:      v.Type,
		At:        v.At,
	}, "", nil
}

type change struct {
	OrderID  string    `json:"order_id"`
	Status   string    `json:"status"`
	Previous string    `json:"previous"`
	At       time.Time `json:"at"`
}

func encodeChange(orderID string, c Change) []byte {
	b, err := json.Marshal(change{OrderID: orderID, Status: c.Status, Previous: c.Previous, At: c.At})
	if err != nil {
		panic(err)
	}
	return b
}

func (p *Projector) deadLetter(ctx context.Context, m kafka.Message, from place, reason string, cause error, attempts int) error {
	headers := make([]kafka.Header, 0, len(m.Headers)+6)
	headers = append(headers, m.Headers...)
	headers = append(headers,
		kafka.Header{Key: "dlq.reason", Value: []byte(reason)},
		kafka.Header{Key: "dlq.error", Value: []byte(cause.Error())},
		kafka.Header{Key: "dlq.topic", Value: []byte(from.topic)},
		kafka.Header{Key: "dlq.partition", Value: []byte(strconv.FormatInt(int64(from.partition), 10))},
		kafka.Header{Key: "dlq.offset", Value: []byte(strconv.FormatInt(from.offset, 10))},
	)
	if attempts > 0 {
		headers = append(headers, kafka.Header{Key: "dlq.attempts", Value: []byte(strconv.Itoa(attempts))})
	}
	err := p.deadLetters.Produce(ctx, kafka.Record{
		Topic:   p.settings.DeadLetterTopic,
		Key:     m.Key,
		Value:   m.Value,
		Headers: headers,
	})
	if err != nil {
		return fmt.Errorf("dead-lettering %s, partition %d, offset %d as %s: %w", from.topic, from.partition, from.offset, reason, err)
	}
	p.dropped.Add(ctx, 1, metric.WithAttributes(reasonKey.String(reason)))
	return nil
}
