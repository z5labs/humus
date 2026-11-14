//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/rand"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-amqp/v3/pkg/amqp"
	"github.com/stretchr/testify/require"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/z5labs/humus/example/rabbitmq/watermill/cases"
)

const rabbitmqImage = "docker.io/library/rabbitmq:4.3.6"

func TestRouter_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			b := startRabbitMQ(t)
			invoicer := b.addUser(t, ".*", ".*", ".*")
			run(t, (&url.URL{Scheme: "amqp", User: url.UserPassword(invoicer.username, invoicer.password), Host: b.addr, Path: "/"}).String())

			c.Run(t, cases.Target{
				Addr:               b.addr,
				Vhost:              "/",
				Username:           b.container.AdminUsername,
				Password:           b.container.AdminPassword,
				OrdersExchange:     ordersTopic,
				InvoicesExchange:   invoicesTopic,
				DeadLetterExchange: poisonTopic,
				Timeout:            time.Minute,
			})
		})
	}
}

func run(t *testing.T, amqpURI string) {
	t.Helper()
	logger := watermill.NopLogger{}
	config := newConfig(amqpURI)
	subscriber, err := amqp.NewSubscriber(config, logger)
	require.NoError(t, err)
	publisher, err := amqp.NewPublisher(config, logger)
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

type login struct {
	username string
	password string
}

type rabbitMQ struct {
	container *tcrabbitmq.RabbitMQContainer
	addr      string
}

func startRabbitMQ(t *testing.T) *rabbitMQ {
	t.Helper()
	c, err := tcrabbitmq.Run(t.Context(), rabbitmqImage,
		tcrabbitmq.WithAdminUsername("admin-"+strings.ToLower(rand.Text())),
		tcrabbitmq.WithAdminPassword(rand.Text()),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting RabbitMQ in a container")
	addr, err := c.PortEndpoint(t.Context(), tcrabbitmq.DefaultAMQPPort, "")
	require.NoError(t, err)
	return &rabbitMQ{container: c, addr: addr}
}

func (r *rabbitMQ) addUser(t *testing.T, configure, write, read string) login {
	t.Helper()
	l := login{username: strings.ToLower(rand.Text()), password: rand.Text()}
	r.ctl(t, "add_user", l.username, l.password)
	r.ctl(t, "set_permissions", "--vhost", "/", l.username, configure, write, read)
	return l
}

func (r *rabbitMQ) ctl(t *testing.T, args ...string) {
	t.Helper()
	code, out, err := r.container.Exec(t.Context(), append([]string{"rabbitmqctl"}, args...), tcexec.Multiplexed())
	require.NoError(t, err, "rabbitmqctl %s", args[0])
	b, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, "rabbitmqctl %s: %s", args[0], b)
}
