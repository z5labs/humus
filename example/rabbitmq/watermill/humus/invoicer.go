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

	"github.com/z5labs/humus/queue/rabbitmq"
)

const (
	headerCorrelationID = "correlation_id"
	headerAttempt       = "retry.attempt"
	headerError         = "retry.error"
	headerDeadError     = "dlq.error"
	headerDeadAttempts  = "dlq.attempts"
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

func issueInvoice(body []byte) ([]byte, error) {
	var o order
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}
	return json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
}

type publisher interface {
	Publish(ctx context.Context, p rabbitmq.Publishing) error
}

type invoicer struct {
	publisher publisher
	attempts  int
}

func (i *invoicer) Process(ctx context.Context, m rabbitmq.Message) error {
	attempt, err := attemptOf(m)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	body, err := issueInvoice(m.Body)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	err = i.publisher.Publish(ctx, rabbitmq.Publishing{
		Exchange: invoicesExchange,
		Headers:  correlationID(m),
		Body:     body,
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	return i.retryLater(ctx, m, attempt, err)
}

func attemptOf(m rabbitmq.Message) (int, error) {
	v, ok := m.Headers[headerAttempt]
	if !ok {
		return 0, nil
	}
	s, _ := v.(string)
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("the %s header %v is not an attempt", headerAttempt, v)
	}
	return n, nil
}

func correlationID(m rabbitmq.Message) map[string]any {
	v, ok := m.Headers[headerCorrelationID]
	if !ok {
		return nil
	}
	return map[string]any{headerCorrelationID: v}
}

func orderHeaders(m rabbitmq.Message) map[string]any {
	headers := make(map[string]any, len(m.Headers)+2)
	for k, v := range m.Headers {
		if !strings.HasPrefix(k, "x-") && !strings.HasPrefix(k, "retry.") {
			headers[k] = v
		}
	}
	return headers
}

func (i *invoicer) retryLater(ctx context.Context, m rabbitmq.Message, attempt int, cause error) error {
	failed := attempt + 1
	if failed >= i.attempts {
		return i.deadLetter(ctx, m, cause, failed)
	}

	headers := orderHeaders(m)
	headers[headerAttempt] = strconv.Itoa(failed)
	headers[headerError] = cause.Error()
	err := i.publisher.Publish(ctx, rabbitmq.Publishing{Exchange: retryExchange, Headers: headers, Body: m.Body})
	if err != nil {
		return fmt.Errorf("parking the order in %s: %w", retryExchange, err)
	}
	return nil
}

func (i *invoicer) deadLetter(ctx context.Context, m rabbitmq.Message, cause error, attempts int) error {
	headers := orderHeaders(m)
	headers[headerDeadError] = cause.Error()
	if attempts > 0 {
		headers[headerDeadAttempts] = strconv.Itoa(attempts)
	}
	err := i.publisher.Publish(ctx, rabbitmq.Publishing{Exchange: deadLetterExchange, Headers: headers, Body: m.Body})
	if err != nil {
		return fmt.Errorf("dead-lettering the order to %s: %w", deadLetterExchange, err)
	}
	return nil
}
