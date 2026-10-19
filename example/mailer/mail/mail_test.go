// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package mail_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/queue/rabbitmq"
)

func delivery(body string) rabbitmq.Message {
	return rabbitmq.Message{
		Queue:         mail.SendQueue,
		Exchange:      mail.Exchange,
		RoutingKey:    mail.SendKey,
		MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		CorrelationID: "order-42",
		ContentType:   "application/json",
		Body:          []byte(body),
	}
}

func TestDecode_a_valid_request(t *testing.T) {
	r, err := mail.Decode(delivery(`{"to":"ada@example.com","subject":"Your order","text":"It shipped."}`))

	require.NoError(t, err)
	require.Equal(t, mail.Request{
		MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		CorrelationID: "order-42",
		To:            "ada@example.com",
		Subject:       "Your order",
		Text:          "It shipped.",
	}, r)
}

func TestDecode_a_request_without_a_CorrelationId(t *testing.T) {
	m := delivery(`{"to":"ada@example.com","subject":"Your order","text":"It shipped."}`)
	m.CorrelationID = ""

	r, err := mail.Decode(m)

	require.NoError(t, err)
	require.Empty(t, r.CorrelationID)
}

func TestDecode_a_request_with_fields_it_does_not_know(t *testing.T) {
	r, err := mail.Decode(delivery(`{"to":"ada@example.com","subject":"Your order","text":"It shipped.","order":42,"tags":["shipping"]}`))

	require.NoError(t, err)
	require.Equal(t, "ada@example.com", r.To)
	require.Equal(t, "Your order", r.Subject)
	require.Equal(t, "It shipped.", r.Text)
}

func TestDecode_a_body_of_256_KiB(t *testing.T) {
	m := delivery(`{"to":"ada@example.com","subject":"Your order","text":""}`)
	text := strings.Repeat("a", mail.MaxBodySize-len(m.Body))
	m.Body = []byte(`{"to":"ada@example.com","subject":"Your order","text":"` + text + `"}`)
	require.Len(t, m.Body, mail.MaxBodySize)

	r, err := mail.Decode(m)

	require.NoError(t, err)
	require.Equal(t, text, r.Text)
}

func TestDecode_a_malformed_request(t *testing.T) {
	valid := `{"to":"ada@example.com","subject":"Your order","text":"It shipped."}`
	cases := map[string]struct {
		message rabbitmq.Message
		says    string
	}{
		"no MessageId": {
			message: func() rabbitmq.Message {
				m := delivery(valid)
				m.MessageID = ""
				return m
			}(),
			says: "no MessageId",
		},
		"not JSON": {
			message: delivery(`to=ada@example.com`),
			says:    "not a JSON object",
		},
		"a JSON array": {
			message: delivery(`["ada@example.com","Your order","It shipped."]`),
			says:    "not a JSON object",
		},
		"a field that is not a string": {
			message: delivery(`{"to":"ada@example.com","subject":42,"text":"It shipped."}`),
			says:    "subject and text as strings",
		},
		"JSON followed by more": {
			message: delivery(valid + `{}`),
			says:    "not a JSON object",
		},
		"no to": {
			message: delivery(`{"subject":"Your order","text":"It shipped."}`),
			says:    "no to",
		},
		"an empty to": {
			message: delivery(`{"to":"","subject":"Your order","text":"It shipped."}`),
			says:    "no to",
		},
		"no subject": {
			message: delivery(`{"to":"ada@example.com","text":"It shipped."}`),
			says:    "no subject",
		},
		"no text": {
			message: delivery(`{"to":"ada@example.com","subject":"Your order"}`),
			says:    "no text",
		},
		"over 256 KiB": {
			message: delivery(`{"to":"ada@example.com","subject":"Your order","text":"` + strings.Repeat("a", mail.MaxBodySize) + `"}`),
			says:    "over 262144",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := mail.Decode(c.message)

			require.ErrorIs(t, err, mail.ErrMalformed)
			require.ErrorContains(t, err, c.says)
		})
	}
}

func TestRequest_Sent(t *testing.T) {
	r := mail.Request{MessageID: "7f6c1f0e", CorrelationID: "order-42"}

	p := r.Sent("prov-1")

	require.Equal(t, mail.EventsExchange, p.Exchange)
	require.Equal(t, mail.SentKey, p.RoutingKey)
	require.Equal(t, "7f6c1f0e", p.MessageID)
	require.Equal(t, "order-42", p.CorrelationID)
	require.Equal(t, "application/json", p.ContentType)
	require.False(t, p.Transient)
	require.JSONEq(t, `{"provider_id":"prov-1"}`, string(p.Body))
}

func TestRequest_Refused(t *testing.T) {
	r := mail.Request{MessageID: "7f6c1f0e"}

	p := r.Refused(`no such domain "nowhere.invalid"`)

	require.Equal(t, mail.EventsExchange, p.Exchange)
	require.Equal(t, mail.RefusedKey, p.RoutingKey)
	require.Equal(t, "7f6c1f0e", p.MessageID)
	require.Empty(t, p.CorrelationID)
	require.Equal(t, "application/json", p.ContentType)
	var body map[string]string
	require.NoError(t, json.Unmarshal(p.Body, &body))
	require.Equal(t, map[string]string{"reason": `no such domain "nowhere.invalid"`}, body)
}
