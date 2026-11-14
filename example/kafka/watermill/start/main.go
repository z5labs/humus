// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command start is the invoicer the Watermill migration guide starts from: a Watermill Router on Kafka.
package main

import (
	"context"
	"os"
	"strings"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
)

func main() {
	logger := watermill.NewStdLogger(false, false)
	brokers := strings.Split(os.Getenv("KAFKA_BROKERS"), ",")

	subscriber, err := newSubscriber(brokers, logger)
	if err != nil {
		panic(err)
	}
	publisher, err := newPublisher(brokers, logger)
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
