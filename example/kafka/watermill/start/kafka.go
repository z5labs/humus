// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
)

const consumerGroup = "invoicer"

func newSubscriber(brokers []string, logger watermill.LoggerAdapter) (*kafka.Subscriber, error) {
	saramaConfig := kafka.DefaultSaramaSubscriberConfig()
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest

	return kafka.NewSubscriber(kafka.SubscriberConfig{
		Brokers:               brokers,
		Unmarshaler:           kafka.DefaultMarshaler{},
		OverwriteSaramaConfig: saramaConfig,
		ConsumerGroup:         consumerGroup,
	}, logger)
}

func newPublisher(brokers []string, logger watermill.LoggerAdapter) (*kafka.Publisher, error) {
	return kafka.NewPublisher(kafka.PublisherConfig{
		Brokers:   brokers,
		Marshaler: kafka.DefaultMarshaler{},
	}, logger)
}
