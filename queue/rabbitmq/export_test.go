// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"io"
)

// Connect is the connecting code the Service and the Publisher share.
var Connect = connect

// Channel is what the Service uses of an AMQP channel.
type Channel = channel

// SetOpen replaces how svc opens a connection and a channel, so a unit test
// hands it a stand-in for amqp091's channel.
func SetOpen(svc *Service, open func(ctx context.Context) (Channel, io.Closer, error)) {
	svc.open = open
}

// PublishChannel is what the Publisher uses of an AMQP channel.
type PublishChannel = publishChannel

// PublishConn is what the Publisher uses of an AMQP connection.
type PublishConn = publishConn

// SetDial replaces how p connects to the broker, so a unit test hands it a
// stand-in for amqp091's connection, whose channels are stand-ins too.
func SetDial(p *Publisher, dial func(ctx context.Context) (PublishConn, error)) {
	p.dial = dial
}

// Classify is the Publisher's classifier for retry.
var Classify = classify

// ClassifyBreaker is the Publisher's breaker's classifier.
var ClassifyBreaker = classifyBreaker

// StripDelivery is svc's strip of the failure scenario of a delivery, m,
// whose context under its process span is ctx.
func StripDelivery(svc *Service, ctx context.Context, m Message) context.Context {
	return svc.strip(ctx, m)
}
