// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package mail is the mailer's contract with other services, and its topology's names.
package mail

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/z5labs/humus/queue/rabbitmq"
)

const (
	Exchange       = "mail"
	RetryExchange  = "mail.retry"
	DeadExchange   = "mail.dead"
	EventsExchange = "mail.events"
)

const (
	SendQueue  = "mail.send"
	RetryQueue = "mail.send.retry"
	DeadQueue  = "mail.send.dead"
)

const (
	SendKey    = "mail.send"
	SentKey    = "mail.sent"
	RefusedKey = "mail.refused"
)

const (
	AttemptHeader      = "x-mail-attempt"
	DeadReasonHeader   = "x-mail-dead-reason"
	DeadAttemptsHeader = "x-mail-dead-attempts"
	DeadErrorHeader    = "x-mail-dead-error"
)

const MaxBodySize = 256 << 10

var ErrMalformed = errors.New("malformed request")

type Request struct {
	MessageID     string
	CorrelationID string
	To            string
	Subject       string
	Text          string
}

type body struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

func Decode(m rabbitmq.Message) (Request, error) {
	if m.MessageID == "" {
		return Request{}, fmt.Errorf("%w: it has no MessageId", ErrMalformed)
	}
	if len(m.Body) > MaxBodySize {
		return Request{}, fmt.Errorf("%w: its body is %d bytes, over %d", ErrMalformed, len(m.Body), MaxBodySize)
	}
	var b body
	if err := json.Unmarshal(m.Body, &b); err != nil {
		return Request{}, fmt.Errorf("%w: its body is not a JSON object of strings: %w", ErrMalformed, err)
	}
	for _, f := range []struct{ name, value string }{
		{"to", b.To},
		{"subject", b.Subject},
		{"text", b.Text},
	} {
		if f.value == "" {
			return Request{}, fmt.Errorf("%w: its body has no %s", ErrMalformed, f.name)
		}
	}
	return Request{
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		To:            b.To,
		Subject:       b.Subject,
		Text:          b.Text,
	}, nil
}

type sent struct {
	ProviderID string `json:"provider_id"`
}

type refused struct {
	Reason string `json:"reason"`
}

func (r Request) Sent(providerID string) rabbitmq.Publishing {
	return r.event(SentKey, sent{ProviderID: providerID})
}

func (r Request) Refused(reason string) rabbitmq.Publishing {
	return r.event(RefusedKey, refused{Reason: reason})
}

func (r Request) event(key string, v any) rabbitmq.Publishing {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return rabbitmq.Publishing{
		Exchange:      EventsExchange,
		RoutingKey:    key,
		ContentType:   "application/json",
		MessageID:     r.MessageID,
		CorrelationID: r.CorrelationID,
		Body:          b,
	}
}
