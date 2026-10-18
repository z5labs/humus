//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// publishWithBaggage publishes a persistent message with id to queue,
// through the default exchange, with a baggage header of bag and the
// user-id userID, and waits for the broker to confirm it. The admin
// channel's connection logs in as the broker's user, so RabbitMQ accepts
// only that user, or none, as userID.
func publishWithBaggage(t *testing.T, queue, id, userID, bag string) {
	t.Helper()
	dc, err := adminChannel(t).PublishWithDeferredConfirmWithContext(t.Context(), "", queue, true, false, amqp.Publishing{
		MessageId:    id,
		UserId:       userID,
		DeliveryMode: amqp.Persistent,
		Headers:      amqp.Table{"baggage": bag},
		Body:         []byte(id),
	})
	require.NoError(t, err)
	ok, err := dc.WaitContext(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "the broker nacked %s", id)
}

func TestService_strips_the_scenario_of_an_untrusted_delivery_and_keeps_one_trusted_by_its_UserID(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	user := rabbit(t).username
	bag := scenarioBaggage(t, abortAccepted)
	publishWithBaggage(t, q, "untrusted", "", bag)
	publishWithBaggage(t, q, "trusted", user, bag)

	codec := faultsCodec(t)
	var (
		mu   sync.Mutex
		seen []processed
		done = make(chan struct{})
	)
	settings := consumerSettings(plainText(t), q)
	settings.TrustScenario = func(m rabbitmq.Message) bool {
		return m.UserID == user
	}
	tel := humustest.NewTelemetry()
	p, status := startApp(t, settings, queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
		_, carried, _ := fit.ScenarioFromContext(ctx, codec)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, processed{
			id:       m.MessageID,
			scenario: carried,
			tenant:   baggage.FromContext(ctx).Member("tenant").Value(),
		})
		if len(seen) == 2 {
			close(done)
		}
		return nil
	}), humustest.WithApp(humus.WithTelemetry(tel.Builder()), withFaults(t)))

	select {
	case <-done:
	case st := <-status:
		t.Fatalf("the App exited before processing both deliveries: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the deliveries were never both processed")
	}
	p.Signal(syscall.SIGTERM)
	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	mu.Lock()
	require.Equal(t, []processed{
		{id: "untrusted", scenario: false, tenant: "acme"},
		{id: "trusted", scenario: true, tenant: "acme"},
	}, seen, "every other baggage member is kept")
	mu.Unlock()

	strips := map[string]int{}
	for _, span := range spansNamed(tel, "process "+q) {
		evs := spanEvents(span, stripEvent)
		for _, e := range evs {
			require.Empty(t, e.Attributes)
		}
		strips[spanAttrs(span)["messaging.message.id"].AsString()] = len(evs)
	}
	require.Equal(t, map[string]int{"untrusted": 1, "trusted": 0}, strips)
}
