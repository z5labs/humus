// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Message is a delivery as a Processor sees it: where it came from, its
// properties and its body.
type Message struct {
	// Queue is the queue it was consumed from.
	Queue string

	// Exchange is the exchange it was published to, "" for the default
	// exchange.
	Exchange string

	// RoutingKey is the routing key it was published with.
	RoutingKey string

	// Redelivered is true when the broker delivered it before, to this
	// consumer or another.
	Redelivered bool

	// Headers are its headers, the values of an AMQP field table as amqp091
	// decodes them: a nested table is an amqp.Table, and a decimal an
	// amqp.Decimal.
	Headers map[string]any

	// ContentType is its MIME content type.
	ContentType string

	// ContentEncoding is its MIME content encoding.
	ContentEncoding string

	// Persistent is true when it was published persistent: with delivery
	// mode 2, so the broker writes it to disk.
	Persistent bool

	// Priority is its priority, from 0 to 9.
	Priority uint8

	// CorrelationID is the correlation ID it was published with.
	CorrelationID string

	// ReplyTo is the address to reply to.
	ReplyTo string

	// Expiration is its expiration, in milliseconds, as the publisher set
	// it.
	Expiration string

	// MessageID is the ID it was published with.
	MessageID string

	// Timestamp is the time it was published with.
	Timestamp time.Time

	// Type is its type name.
	Type string

	// UserID is the user that published it, when the publisher set it.
	UserID string

	// AppID is the ID of the application that published it.
	AppID string

	// Body is its body.
	Body []byte
}

// newMessage returns the Message of d, a delivery from queue.
func newMessage(queue string, d amqp.Delivery) Message {
	var headers map[string]any
	if d.Headers != nil {
		headers = map[string]any(d.Headers)
	}
	return Message{
		Queue:           queue,
		Exchange:        d.Exchange,
		RoutingKey:      d.RoutingKey,
		Redelivered:     d.Redelivered,
		Headers:         headers,
		ContentType:     d.ContentType,
		ContentEncoding: d.ContentEncoding,
		Persistent:      d.DeliveryMode == amqp.Persistent,
		Priority:        d.Priority,
		CorrelationID:   d.CorrelationId,
		ReplyTo:         d.ReplyTo,
		Expiration:      d.Expiration,
		MessageID:       d.MessageId,
		Timestamp:       d.Timestamp,
		Type:            d.Type,
		UserID:          d.UserId,
		AppID:           d.AppId,
		Body:            d.Body,
	}
}
