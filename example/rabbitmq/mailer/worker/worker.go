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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/example/rabbitmq/mailer/mail"
	"github.com/z5labs/humus/example/rabbitmq/mailer/provider"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

const scope = "github.com/z5labs/humus/example/rabbitmq/mailer/worker"

const (
	malformed = "malformed"
	gaveUp    = "gave-up"
)

const (
	attemptKey = attribute.Key("mail.attempt")
	outcomeKey = attribute.Key("mail.outcome")
)

const (
	outcomeSent      = "sent"
	outcomeRefused   = "refused"
	outcomeRetried   = "retried"
	outcomeGaveUp    = "gave_up"
	outcomeMalformed = "malformed"
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
	DeadLetters Publisher
	MaxAttempts int
	User        string
	Telemetry   telemetry.SDK
}

type Worker struct {
	provider    Provider
	retries     Publisher
	results     Publisher
	deadLetters Publisher
	maxAttempts int
	user        string
	logger      log.Logger
	requests    metric.Int64Counter
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
	if settings.DeadLetters == nil {
		errs = append(errs, errors.New("Settings.DeadLetters must not be nil"))
	}
	if settings.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("Settings.MaxAttempts must be positive, not %d", settings.MaxAttempts))
	}
	if settings.User == "" {
		errs = append(errs, errors.New("Settings.User must not be empty"))
	}
	if settings.Telemetry.MeterProvider == nil {
		errs = append(errs, errors.New("Settings.Telemetry.MeterProvider must not be nil"))
	}
	if settings.Telemetry.LoggerProvider == nil {
		errs = append(errs, errors.New("Settings.Telemetry.LoggerProvider must not be nil"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("worker: New: %w", err)
	}
	requests, err := settings.Telemetry.MeterProvider.Meter(scope).Int64Counter("mail.requests",
		metric.WithDescription("Mail requests processed, by outcome."),
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, fmt.Errorf("worker: New: %w", err)
	}
	return &Worker{
		provider:    settings.Provider,
		retries:     settings.Retries,
		results:     settings.Results,
		deadLetters: settings.DeadLetters,
		maxAttempts: settings.MaxAttempts,
		user:        settings.User,
		logger:      settings.Telemetry.LoggerProvider.Logger(scope),
		requests:    requests,
	}, nil
}

func (w *Worker) Process(ctx context.Context, m rabbitmq.Message) error {
	outcome, attempt, err := w.process(ctx, m)
	if err != nil {
		return err
	}
	w.observe(ctx, m, outcome, attempt)
	return nil
}

func (w *Worker) process(ctx context.Context, m rabbitmq.Message) (outcome string, attempt int, err error) {
	span := trace.SpanFromContext(ctx)
	attempt, err = attemptOf(m)
	if err != nil {
		span.SetAttributes(attemptKey.Int(1))
		return outcomeMalformed, 1, w.deadLetter(ctx, m, malformed, 1, err)
	}
	span.SetAttributes(attemptKey.Int(attempt))
	r, err := mail.Decode(m)
	if err != nil {
		return outcomeMalformed, attempt, w.deadLetter(ctx, m, malformed, attempt, err)
	}
	refused, err := w.send(ctx, r)
	switch {
	case err == nil && refused:
		return outcomeRefused, attempt, nil
	case err == nil:
		return outcomeSent, attempt, nil
	case attempt >= w.maxAttempts:
		return outcomeGaveUp, attempt, w.deadLetter(ctx, m, gaveUp, attempt, err)
	default:
		return outcomeRetried, attempt, w.park(ctx, m, attempt+1)
	}
}

func (w *Worker) send(ctx context.Context, r mail.Request) (refused bool, err error) {
	res, err := w.provider.Send(ctx, r)
	if err != nil {
		return false, err
	}
	event := r.Sent(res.ID)
	if res.Refused {
		event = r.Refused(res.Reason)
	}
	err = w.results.Publish(ctx, event)
	if err != nil && !errors.Is(err, rabbitmq.ErrUnroutable) {
		return false, fmt.Errorf("worker: publishing %s: %w", event.RoutingKey, err)
	}
	return res.Refused, nil
}

func (w *Worker) observe(ctx context.Context, m rabbitmq.Message, outcome string, attempt int) {
	trace.SpanFromContext(ctx).SetAttributes(outcomeKey.String(outcome))
	w.requests.Add(ctx, 1, metric.WithAttributes(outcomeKey.String(outcome)))

	severity, text := log.SeverityInfo, "INFO"
	if outcome == outcomeGaveUp || outcome == outcomeMalformed {
		severity, text = log.SeverityWarn, "WARN"
	}
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(severity)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue("mail request " + outcome))
	if m.MessageID != "" {
		r.AddAttributes(semconv.MessagingMessageID(m.MessageID))
	}
	r.AddAttributes(attemptKey.Int(attempt), outcomeKey.String(outcome))
	w.logger.Emit(ctx, r)
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
	if err := w.deadLetters.Publish(ctx, p); err != nil {
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
