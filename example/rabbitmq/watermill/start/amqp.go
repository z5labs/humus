// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"github.com/ThreeDotsLabs/watermill-amqp/v3/pkg/amqp"
)

const consumerGroup = "invoicer"

func newConfig(amqpURI string) amqp.Config {
	return amqp.NewDurablePubSubConfig(amqpURI, amqp.GenerateQueueNameTopicNameWithSuffix(consumerGroup))
}
