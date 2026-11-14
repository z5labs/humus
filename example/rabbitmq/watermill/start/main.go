// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command start is the invoicer the Watermill migration guide starts from: a Watermill Router on RabbitMQ.
package main

import (
	"context"
	"os"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-amqp/v3/pkg/amqp"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
)

func main() {
	logger := watermill.NewStdLogger(false, false)
	config := newConfig(os.Getenv("AMQP_URI"))

	subscriber, err := amqp.NewSubscriber(config, logger)
	if err != nil {
		panic(err)
	}
	publisher, err := amqp.NewPublisher(config, logger)
	if err != nil {
		panic(err)
	}

	router, err := newRouter(subscriber, publisher, logger)
	if err != nil {
		panic(err)
	}
	router.AddPlugin(plugin.SignalsHandler)

	if err := router.Run(context.Background()); err != nil {
		panic(err)
	}
	if err := publisher.Close(); err != nil {
		panic(err)
	}
}
