// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
)

const (
	ordersTopic   = "orders"
	invoicesTopic = "invoices"
	poisonTopic   = "orders.poison"
)

func newRouter(subscriber message.Subscriber, publisher message.Publisher, logger watermill.LoggerAdapter) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return nil, err
	}

	poisonQueue, err := middleware.PoisonQueue(publisher, poisonTopic)
	if err != nil {
		return nil, err
	}

	router.AddMiddleware(
		middleware.CorrelationID,
		poisonQueue,
		middleware.Retry{
			MaxRetries:      3,
			InitialInterval: 100 * time.Millisecond,
			MaxInterval:     time.Second,
			Multiplier:      2,
			Logger:          logger,
		}.Middleware,
		middleware.Recoverer,
	)

	router.AddHandler("invoicer", ordersTopic, subscriber, invoicesTopic, publisher, issueInvoice)
	return router, nil
}
