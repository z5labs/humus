// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package worker decides what becomes of each mail request the mailer consumes.
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/example/mailer/provider"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

const (
	malformed = "malformed"
	gaveUp    = "gave-up"
)

type Provider interface {
	Send(ctx context.Context, r mail.Request) (provider.Result, error)
}

type Publisher interface {
	Publish(ctx context.Context, m rabbitmq.Publishing) error
}

type Settings struct {
	Provider    Provider
	Retries     Publisher
	Results     Publisher
	MaxAttempts int
	User        string
}

type Worker struct {
	provider    Provider
	retries     Publisher
	results     Publisher
	maxAttempts int
	user        string
}

var _ queue.Processor[rabbitmq.Message] = (*Worker)(nil)

func New(settings Settings) (*Worker, error) {
	var errs []error
	if settings.Provider == nil {
		errs = append(errs, errors.New("Settings.Provider must not be nil"))
	}
	if settings.Retries == nil {
		errs = append(errs, errors.New("Settings.Retries must not be nil"))
	}
	if settings.Results == nil {
		errs = append(errs, errors.New("Settings.Results must not be nil"))
	}
	if settings.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("Settings.MaxAttempts must be positive, not %d", settings.MaxAttempts))
	}
	if settings.User == "" {
		errs = append(errs, errors.New("Settings.User must not be empty"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("worker: New: %w", err)
	}
	return &Worker{
		provider:    settings.Provider,
		retries:     settings.Retries,
		results:     settings.Results,
		maxAttempts: settings.MaxAttempts,
		user:        settings.User,
	}, nil
}

func (w *Worker) Process(ctx context.Context, m rabbitmq.Message) error {
	attempt, err := attemptOf(m)
	if err != nil {
		return w.deadLetter(ctx, m, malformed, 1, err)
	}
	r, err := mail.Decode(m)
	if err != nil {
		return w.deadLetter(ctx, m, malformed, attempt, err)
	}
	err = w.send(ctx, r)
	switch {
	case err == nil:
		return nil
	case attempt >= w.maxAttempts:
		return w.deadLetter(ctx, m, gaveUp, attempt, err)
	default:
		return w.park(ctx, m, attempt+1)
	}
}

func (w *Worker) send(ctx context.Context, r mail.Request) error {
	res, err := w.provider.Send(ctx, r)
	if err != nil {
		return err
	}
	event := r.Sent(res.ID)
	if res.Refused {
		event = r.Refused(res.Reason)
	}
	err = w.results.Publish(ctx, event)
	if err != nil && !errors.Is(err, rabbitmq.ErrUnroutable) {
		return fmt.Errorf("worker: publishing %s: %w", event.RoutingKey, err)
	}
	return nil
}

func (w *Worker) park(ctx context.Context, m rabbitmq.Message, attempt int) error {
	p := request(m, mail.RetryExchange, map[string]any{
		mail.AttemptHeader: int64(attempt),
	})
	p.UserID = w.user
	if err := w.retries.Publish(ctx, p); err != nil {
		return fmt.Errorf("worker: parking the request in %s: %w", mail.RetryExchange, err)
	}
	return nil
}

func (w *Worker) deadLetter(ctx context.Context, m rabbitmq.Message, reason string, attempts int, cause error) error {
	p := request(m, mail.DeadExchange, map[string]any{
		mail.DeadReasonHeader:   reason,
		mail.DeadAttemptsHeader: int64(attempts),
		mail.DeadErrorHeader:    cause.Error(),
	})
	if err := w.results.Publish(ctx, p); err != nil {
		return fmt.Errorf("worker: dead-lettering the request to %s: %w", mail.DeadExchange, err)
	}
	return nil
}

func request(m rabbitmq.Message, exchange string, headers map[string]any) rabbitmq.Publishing {
	return rabbitmq.Publishing{
		Exchange:      exchange,
		RoutingKey:    mail.SendKey,
		Headers:       headers,
		ContentType:   m.ContentType,
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		Body:          m.Body,
	}
}

func attemptOf(m rabbitmq.Message) (int, error) {
	v, ok := m.Headers[mail.AttemptHeader]
	if !ok {
		return 1, nil
	}
	var n int64
	switch v := v.(type) {
	case int8:
		n = int64(v)
	case int16:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	case uint8:
		n = int64(v)
	case uint16:
		n = int64(v)
	case uint32:
		n = int64(v)
	default:
		return 0, fmt.Errorf("%w: its %s is a %T, not an integer", mail.ErrMalformed, mail.AttemptHeader, v)
	}
	if n < 1 {
		return 0, fmt.Errorf("%w: its %s is %d, not positive", mail.ErrMalformed, mail.AttemptHeader, n)
	}
	return int(n), nil
}
