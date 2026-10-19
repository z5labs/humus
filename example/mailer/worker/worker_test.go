// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/example/mailer/provider"
	"github.com/z5labs/humus/example/mailer/worker"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

type fakeProvider struct {
	mu     sync.Mutex
	result provider.Result
	err    error
	sent   []mail.Request
}

func (p *fakeProvider) Send(_ context.Context, r mail.Request) (provider.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, r)
	return p.result, p.err
}

func (p *fakeProvider) calls() []mail.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]mail.Request(nil), p.sent...)
}

type fakePublisher struct {
	mu        sync.Mutex
	fail      func(rabbitmq.Publishing) error
	published []rabbitmq.Publishing
}

func (p *fakePublisher) Publish(_ context.Context, m rabbitmq.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, m)
	if p.fail != nil {
		return p.fail(m)
	}
	return nil
}

func (p *fakePublisher) all() []rabbitmq.Publishing {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]rabbitmq.Publishing(nil), p.published...)
}

func failing(exchange string, err error) func(rabbitmq.Publishing) error {
	return func(m rabbitmq.Publishing) error {
		if m.Exchange == exchange {
			return err
		}
		return nil
	}
}

type harness struct {
	provider    *fakeProvider
	retries     *fakePublisher
	results     *fakePublisher
	deadLetters *fakePublisher
	tel         *humustest.Telemetry
	sdk         telemetry.SDK
	worker      *worker.Worker
}

func newHarness(t *testing.T, maxAttempts int) *harness {
	t.Helper()
	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	h := &harness{
		provider:    &fakeProvider{result: provider.Result{ID: "msg_1"}},
		retries:     &fakePublisher{},
		results:     &fakePublisher{},
		deadLetters: &fakePublisher{},
		tel:         tel,
		sdk:         sdk,
	}
	w, err := worker.New(worker.Settings{
		Provider:    h.provider,
		Retries:     h.retries,
		Results:     h.results,
		DeadLetters: h.deadLetters,
		MaxAttempts: maxAttempts,
		User:        mailerUser,
		Telemetry:   sdk,
	})
	require.NoError(t, err)
	h.worker = w
	return h
}

const mailerUser = "mailer"

const validBody = `{"to":"ada@example.com","subject":"Your order","text":"It shipped."}`

func delivery(body string, headers map[string]any) rabbitmq.Message {
	return rabbitmq.Message{
		Queue:         mail.SendQueue,
		Exchange:      mail.Exchange,
		RoutingKey:    mail.SendKey,
		Headers:       headers,
		MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		CorrelationID: "order-42",
		ContentType:   "application/json",
		Body:          []byte(body),
	}
}

func parked(m rabbitmq.Message, attempt int64) rabbitmq.Publishing {
	return rabbitmq.Publishing{
		Exchange:      mail.RetryExchange,
		RoutingKey:    mail.SendKey,
		Headers:       map[string]any{mail.AttemptHeader: attempt},
		ContentType:   m.ContentType,
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		UserID:        mailerUser,
		Body:          m.Body,
	}
}

func deadLetter(m rabbitmq.Message, reason string, attempts int64, cause string) rabbitmq.Publishing {
	return rabbitmq.Publishing{
		Exchange:    mail.DeadExchange,
		RoutingKey:  mail.SendKey,
		ContentType: m.ContentType,
		Headers: map[string]any{
			mail.DeadReasonHeader:   reason,
			mail.DeadAttemptsHeader: attempts,
			mail.DeadErrorHeader:    cause,
		},
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		Body:          m.Body,
	}
}

func event(t *testing.T, key string, v map[string]string) rabbitmq.Publishing {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return rabbitmq.Publishing{
		Exchange:      mail.EventsExchange,
		RoutingKey:    key,
		ContentType:   "application/json",
		MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		CorrelationID: "order-42",
		Body:          b,
	}
}

func TestNew_rejects_settings_it_cannot_work_with(t *testing.T) {
	cases := map[string]struct {
		settings worker.Settings
		says     string
	}{
		"no provider": {
			settings: worker.Settings{Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.Provider must not be nil",
		},
		"no retries publisher": {
			settings: worker.Settings{Provider: &fakeProvider{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.Retries must not be nil",
		},
		"no results publisher": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.Results must not be nil",
		},
		"no dead-letter publisher": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.DeadLetters must not be nil",
		},
		"no attempts": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.MaxAttempts must be positive, not 0",
		},
		"negative attempts": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: -1, User: mailerUser, Telemetry: telemetry.Noop()},
			says:     "Settings.MaxAttempts must be positive, not -1",
		},
		"no user": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, Telemetry: telemetry.Noop()},
			says:     "Settings.User must not be empty",
		},
		"no meter provider": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: func() telemetry.SDK { s := telemetry.Noop(); s.MeterProvider = nil; return s }()},
			says:     "Settings.Telemetry.MeterProvider must not be nil",
		},
		"no logger provider": {
			settings: worker.Settings{Provider: &fakeProvider{}, Retries: &fakePublisher{}, Results: &fakePublisher{}, DeadLetters: &fakePublisher{}, MaxAttempts: 5, User: mailerUser, Telemetry: func() telemetry.SDK { s := telemetry.Noop(); s.LoggerProvider = nil; return s }()},
			says:     "Settings.Telemetry.LoggerProvider must not be nil",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w, err := worker.New(tc.settings)

			require.Nil(t, w)
			require.ErrorContains(t, err, tc.says)
		})
	}
}

func TestProcess_dead_letters_a_malformed_request_without_calling_the_provider(t *testing.T) {
	cases := map[string]struct {
		message  rabbitmq.Message
		attempts int64
		says     string
	}{
		"no MessageId": {
			message:  func() rabbitmq.Message { m := delivery(validBody, nil); m.MessageID = ""; return m }(),
			attempts: 1,
			says:     "malformed request: it has no MessageId",
		},
		"not JSON": {
			message:  delivery(`not json`, nil),
			attempts: 1,
			says:     "malformed request: its body is not a JSON object",
		},
		"a field missing": {
			message:  delivery(`{"to":"ada@example.com","text":"It shipped."}`, nil),
			attempts: 1,
			says:     "malformed request: its body has no subject",
		},
		"on a later attempt": {
			message:  delivery(`not json`, map[string]any{mail.AttemptHeader: int64(3)}),
			attempts: 3,
			says:     "malformed request: its body is not a JSON object",
		},
		"an attempt that is not an integer": {
			message:  delivery(validBody, map[string]any{mail.AttemptHeader: "two"}),
			attempts: 1,
			says:     "malformed request: its x-mail-attempt is a string, not an integer",
		},
		"an attempt that is not positive": {
			message:  delivery(validBody, map[string]any{mail.AttemptHeader: int32(0)}),
			attempts: 1,
			says:     "malformed request: its x-mail-attempt is 0, not positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, 5)

				err := h.worker.Process(t.Context(), tc.message)

				require.NoError(t, err)
				require.Empty(t, h.provider.calls())
				require.Empty(t, h.retries.all())
				require.Empty(t, h.results.all())
				got := h.deadLetters.all()
				require.Len(t, got, 1)
				require.Contains(t, got[0].Headers[mail.DeadErrorHeader], tc.says)
				require.Equal(t, deadLetter(tc.message, "malformed", tc.attempts, got[0].Headers[mail.DeadErrorHeader].(string)), got[0])
				require.NotContains(t, got[0].Headers, mail.AttemptHeader)
			})
		})
	}
}

func TestProcess_publishes_mail_sent_for_a_sent_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.provider.result = provider.Result{ID: "msg_1"}

		err := h.worker.Process(t.Context(), delivery(validBody, nil))

		require.NoError(t, err)
		require.Equal(t, []mail.Request{{
			MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
			CorrelationID: "order-42",
			To:            "ada@example.com",
			Subject:       "Your order",
			Text:          "It shipped.",
		}}, h.provider.calls())
		require.Equal(t, []rabbitmq.Publishing{event(t, mail.SentKey, map[string]string{"provider_id": "msg_1"})}, h.results.all())
		require.Empty(t, h.retries.all())
		require.Empty(t, h.deadLetters.all())
	})
}

func TestProcess_publishes_mail_refused_for_a_refused_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.provider.result = provider.Result{Refused: true, Reason: "the address does not exist"}

		err := h.worker.Process(t.Context(), delivery(validBody, map[string]any{mail.AttemptHeader: int64(2)}))

		require.NoError(t, err)
		require.Equal(t, []rabbitmq.Publishing{event(t, mail.RefusedKey, map[string]string{"reason": "the address does not exist"})}, h.results.all())
		require.Empty(t, h.retries.all())
		require.Empty(t, h.deadLetters.all())
	})
}

func TestProcess_takes_an_unroutable_event_as_no_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.results.fail = failing(mail.EventsExchange, fmt.Errorf("rabbitmq: publishing to mail.events:mail.sent: %w: 312 NO_ROUTE", rabbitmq.ErrUnroutable))

		err := h.worker.Process(t.Context(), delivery(validBody, nil))

		require.NoError(t, err)
		require.Len(t, h.results.all(), 1)
		require.Empty(t, h.retries.all())
		require.Empty(t, h.deadLetters.all())
	})
}

func TestProcess_parks_a_request_the_provider_failed_below_its_attempts(t *testing.T) {
	cases := map[string]struct {
		headers map[string]any
		next    int64
	}{
		"its first processing":              {headers: nil, next: 2},
		"a header that is not the worker's": {headers: map[string]any{"x-other": "yes"}, next: 2},
		"its second processing":             {headers: map[string]any{mail.AttemptHeader: int64(2)}, next: 3},
		"an attempt as an int32":            {headers: map[string]any{mail.AttemptHeader: int32(3)}, next: 4},
		"an attempt as an int8":             {headers: map[string]any{mail.AttemptHeader: int8(4)}, next: 5},
		"an attempt as an unsigned int16":   {headers: map[string]any{mail.AttemptHeader: uint16(1)}, next: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, 5)
				h.provider.err = errors.New("provider: it answered 503 Service Unavailable")
				m := delivery(validBody, tc.headers)

				err := h.worker.Process(t.Context(), m)

				require.NoError(t, err)
				require.Len(t, h.provider.calls(), 1)
				require.Equal(t, []rabbitmq.Publishing{parked(m, tc.next)}, h.retries.all())
				require.Empty(t, h.results.all())
				require.Empty(t, h.deadLetters.all())
			})
		})
	}
}

func TestProcess_parks_a_request_whose_event_cannot_be_published(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.results.fail = failing(mail.EventsExchange, errors.New("rabbitmq: the broker nacked the publish"))
		m := delivery(validBody, nil)

		err := h.worker.Process(t.Context(), m)

		require.NoError(t, err)
		require.Equal(t, []rabbitmq.Publishing{event(t, mail.SentKey, map[string]string{"provider_id": "msg_1"})}, h.results.all())
		require.Equal(t, []rabbitmq.Publishing{parked(m, 2)}, h.retries.all())
		require.Empty(t, h.deadLetters.all())
	})
}

func TestProcess_dead_letters_a_request_that_failed_on_its_last_attempt(t *testing.T) {
	cases := map[string]struct {
		maxAttempts int
		headers     map[string]any
		attempts    int64
	}{
		"at the attempts":             {maxAttempts: 5, headers: map[string]any{mail.AttemptHeader: int64(5)}, attempts: 5},
		"past the attempts":           {maxAttempts: 3, headers: map[string]any{mail.AttemptHeader: int64(4)}, attempts: 4},
		"a single attempt, its first": {maxAttempts: 1, headers: nil, attempts: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, tc.maxAttempts)
				h.provider.err = errors.New("provider: it answered 503 Service Unavailable")
				m := delivery(validBody, tc.headers)

				err := h.worker.Process(t.Context(), m)

				require.NoError(t, err)
				require.Empty(t, h.retries.all())
				require.Empty(t, h.results.all())
				require.Equal(t, []rabbitmq.Publishing{
					deadLetter(m, "gave-up", tc.attempts, "provider: it answered 503 Service Unavailable"),
				}, h.deadLetters.all())
			})
		})
	}
}

func TestProcess_dead_letters_with_the_event_error_when_the_last_attempt_could_not_publish_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 2)
		h.results.fail = func(rabbitmq.Publishing) error {
			return errors.New("rabbitmq: publishing to exchange \"mail.events\": circuit: call rejected")
		}
		m := delivery(validBody, map[string]any{mail.AttemptHeader: int64(2)})

		err := h.worker.Process(t.Context(), m)

		require.NoError(t, err)
		require.Empty(t, h.retries.all())
		require.Equal(t, []rabbitmq.Publishing{event(t, mail.SentKey, map[string]string{"provider_id": "msg_1"})}, h.results.all())
		require.Equal(t, []rabbitmq.Publishing{
			deadLetter(m, "gave-up", 2, "worker: publishing mail.sent: rabbitmq: publishing to exchange \"mail.events\": circuit: call rejected"),
		}, h.deadLetters.all())
	})
}

func TestProcess_returns_a_failed_publish_to_mail_retry_as_its_error(t *testing.T) {
	cases := map[string]error{
		"a nack":       errors.New("rabbitmq: the broker nacked the publish"),
		"unroutable":   fmt.Errorf("rabbitmq: publishing to mail.retry:mail.send: %w: 312 NO_ROUTE", rabbitmq.ErrUnroutable),
		"its deadline": context.DeadlineExceeded,
	}
	for name, cause := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, 5)
				h.provider.err = errors.New("provider: it answered 503 Service Unavailable")
				h.retries.fail = failing(mail.RetryExchange, cause)

				err := h.worker.Process(t.Context(), delivery(validBody, nil))

				require.ErrorIs(t, err, cause)
				require.ErrorContains(t, err, "worker: parking the request in mail.retry")
				require.Len(t, h.retries.all(), 1)
				require.Empty(t, h.results.all())
				require.Empty(t, h.deadLetters.all())
			})
		})
	}
}

func TestProcess_returns_a_failed_publish_to_mail_dead_as_its_error(t *testing.T) {
	nack := errors.New("rabbitmq: the broker nacked the publish")
	unroutable := fmt.Errorf("rabbitmq: publishing to mail.dead:mail.send: %w: 312 NO_ROUTE", rabbitmq.ErrUnroutable)
	cases := map[string]struct {
		message     rabbitmq.Message
		maxAttempts int
		cause       error
	}{
		"a malformed request, nacked":     {message: delivery(`not json`, nil), maxAttempts: 5, cause: nack},
		"a malformed request, unroutable": {message: delivery(`not json`, nil), maxAttempts: 5, cause: unroutable},
		"a request that gave up, nacked":  {message: delivery(validBody, nil), maxAttempts: 1, cause: nack},
		"a request that gave up, unroutable": {
			message:     delivery(validBody, map[string]any{mail.AttemptHeader: int64(3)}),
			maxAttempts: 3,
			cause:       unroutable,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, tc.maxAttempts)
				h.provider.err = errors.New("provider: it answered 503 Service Unavailable")
				h.deadLetters.fail = failing(mail.DeadExchange, tc.cause)

				err := h.worker.Process(t.Context(), tc.message)

				require.ErrorIs(t, err, tc.cause)
				require.ErrorContains(t, err, "worker: dead-lettering the request to mail.dead")
				require.Empty(t, h.retries.all())
				require.Empty(t, h.results.all())
				require.Len(t, h.deadLetters.all(), 1)
			})
		})
	}
}
