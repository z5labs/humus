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
	"time"

	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/queue/kafka"
)

const (
	reasonMalformed   = "malformed"
	reasonUnknownType = "unknown-type"
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
	StatusTopic     string
	DeadLetterTopic string
}

type Projector struct {
	store       Store
	status      Producer
	deadLetters Producer
	settings    Settings
}

func New(s Store, status, deadLetters Producer, settings Settings) *Projector {
	return &Projector{
		store:       s,
		status:      status,
		deadLetters: deadLetters,
		settings:    settings,
	}
}

func (p *Projector) Process(ctx context.Context, m kafka.Message) error {
	e, reason, err := decode(m)
	if err != nil {
		return p.deadLetter(ctx, m, reason, err)
	}

	applied, err := p.store.Applied(ctx, e.Partition, e.Offset)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	history, err := p.store.Events(ctx, e.OrderID)
	if err != nil {
		return err
	}
	status, changes, err := Fold(history, e)
	if err != nil {
		return err
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
			return fmt.Errorf("producing order %s's changes from partition %d, offset %d: %w", e.OrderID, e.Partition, e.Offset, err)
		}
	}

	err = p.store.Apply(ctx, e, status)
	if errors.Is(err, store.ErrApplied) {
		return nil
	}
	return err
}

type event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

func decode(m kafka.Message) (store.Event, string, error) {
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
		Partition: m.Partition,
		Offset:    m.Offset,
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

func (p *Projector) deadLetter(ctx context.Context, m kafka.Message, reason string, cause error) error {
	headers := make([]kafka.Header, 0, len(m.Headers)+5)
	headers = append(headers, m.Headers...)
	headers = append(headers,
		kafka.Header{Key: "dlq.reason", Value: []byte(reason)},
		kafka.Header{Key: "dlq.error", Value: []byte(cause.Error())},
		kafka.Header{Key: "dlq.topic", Value: []byte(m.Topic)},
		kafka.Header{Key: "dlq.partition", Value: []byte(strconv.FormatInt(int64(m.Partition), 10))},
		kafka.Header{Key: "dlq.offset", Value: []byte(strconv.FormatInt(m.Offset, 10))},
	)
	err := p.deadLetters.Produce(ctx, kafka.Record{
		Topic:   p.settings.DeadLetterTopic,
		Key:     m.Key,
		Value:   m.Value,
		Headers: headers,
	})
	if err != nil {
		return fmt.Errorf("dead-lettering %s, partition %d, offset %d as %s: %w", m.Topic, m.Partition, m.Offset, reason, err)
	}
	return nil
}
