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
