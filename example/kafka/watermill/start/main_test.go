// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/z5labs/humus/example/kafka/watermill/cases"
)

func TestRouter_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, ordersTopic, invoicesTopic, poisonTopic))
			require.NoError(t, err)
			t.Cleanup(cluster.Close)
			run(t, cluster.ListenAddrs())

			c.Run(t, cases.Target{
				Cluster:         cluster,
				Kafka:           []kgo.Opt{kgo.SeedBrokers(cluster.ListenAddrs()...)},
				Group:           consumerGroup,
				Consumes:        []string{ordersTopic},
				OrdersTopic:     ordersTopic,
				InvoicesTopic:   invoicesTopic,
				DeadLetterTopic: poisonTopic,
				Timeout:         30 * time.Second,
			})
		})
	}
}

func run(t *testing.T, brokers []string) {
	t.Helper()
	logger := watermill.NopLogger{}
	subscriber, err := newSubscriber(brokers, logger)
	require.NoError(t, err)
	publisher, err := newPublisher(brokers, logger)
	require.NoError(t, err)
	router, err := newRouter(subscriber, publisher, logger)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- router.Run(context.Background()) }()
	t.Cleanup(func() {
		require.NoError(t, router.Close())
		require.NoError(t, <-done)
		require.NoError(t, publisher.Close())
	})
	<-router.Running()
}
