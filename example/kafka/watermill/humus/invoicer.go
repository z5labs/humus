// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/humus/queue/kafka"
)

const (
	headerCorrelationID = "correlation_id"
	headerAttempt       = "retry.attempt"
	headerError         = "retry.error"
)

type order struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Total    int    `json:"total"`
}

type invoice struct {
	OrderID  string `json:"order_id"`
	Customer string `json:"customer"`
	Amount   int    `json:"amount"`
}

func issueInvoice(value []byte) ([]byte, error) {
	var o order
	if err := json.Unmarshal(value, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}
	return json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
}

type invoicer struct {
	producer *kafka.Producer
	retry    retry
}

func (i *invoicer) Process(ctx context.Context, m kafka.Message) error {
	attempt, err := attemptOf(m)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	payload, err := issueInvoice(m.Value)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	if err := i.wait(ctx, m, attempt); err != nil {
		return err
	}

	err = i.producer.Produce(ctx, kafka.Record{
		Topic:   invoicesTopic,
		Key:     m.Key,
		Value:   payload,
		Headers: correlationID(m),
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	return i.retryLater(ctx, m, attempt, err)
}

func attemptOf(m kafka.Message) (int, error) {
	if m.Topic != retryTopic {
		return 0, nil
	}
	for _, h := range m.Headers {
		if h.Key == headerAttempt {
			n, err := strconv.Atoi(string(h.Value))
			if err != nil || n < 1 {
				return 0, fmt.Errorf("the %s header %q is not an attempt", headerAttempt, h.Value)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("the record has no %s header", headerAttempt)
}

func correlationID(m kafka.Message) []kafka.Header {
	for _, h := range m.Headers {
		if h.Key == headerCorrelationID {
			return []kafka.Header{h}
		}
	}
	return nil
}

func (i *invoicer) wait(ctx context.Context, m kafka.Message, attempt int) error {
	if attempt == 0 {
		return nil
	}
	t := time.NewTimer(time.Until(m.Timestamp.Add(i.retry.Delay)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

func (i *invoicer) retryLater(ctx context.Context, m kafka.Message, attempt int, cause error) error {
	failed := attempt + 1
	if failed >= i.retry.Attempts {
		return i.deadLetter(ctx, m, cause, failed)
	}

	headers := make([]kafka.Header, 0, len(m.Headers)+2)
	for _, h := range m.Headers {
		if !strings.HasPrefix(h.Key, "retry.") {
			headers = append(headers, h)
		}
	}
	headers = append(headers,
		kafka.Header{Key: headerAttempt, Value: []byte(strconv.Itoa(failed))},
		kafka.Header{Key: headerError, Value: []byte(cause.Error())},
	)
	err := i.producer.Produce(ctx, kafka.Record{Topic: retryTopic, Key: m.Key, Value: m.Value, Headers: headers})
	if err != nil {
		return fmt.Errorf("retrying %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, err)
	}
	return nil
}

func (i *invoicer) deadLetter(ctx context.Context, m kafka.Message, cause error, attempts int) error {
	headers := make([]kafka.Header, 0, len(m.Headers)+2)
	headers = append(headers, m.Headers...)
	headers = append(headers, kafka.Header{Key: "dlq.error", Value: []byte(cause.Error())})
	if attempts > 0 {
		headers = append(headers, kafka.Header{Key: "dlq.attempts", Value: []byte(strconv.Itoa(attempts))})
	}
	err := i.producer.Produce(ctx, kafka.Record{Topic: deadLetterTopic, Key: m.Key, Value: m.Value, Headers: headers})
	if err != nil {
		return fmt.Errorf("dead-lettering %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, err)
	}
	return nil
}
