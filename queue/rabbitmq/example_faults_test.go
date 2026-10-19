// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"fmt"

	"github.com/z5labs/humus/queue/rabbitmq"
)

// The failure injection page and the RabbitMQ page of the docs site quote
// the code in this file, and TestDocs_code_samples checks that every quote
// still matches it. TestPublisher_relay_names_the_user_its_Publisher_logs_in_as
// runs relay.

// A Service trusts the failure scenarios of the end user's own services,
// which connect as the user orders-api. RabbitMQ validates a message's
// user-id against the publishing connection's user, so no other publisher
// can claim it. The user-id comes only from the publisher: a humus service
// whose Publisher logs in as orders-api sets Publishing.UserID to
// orders-api on each Publish, as relay does, and a message published
// without one has an empty UserID.
func ExampleSettings_trustScenario() {
	settings := rabbitmq.DefaultSettings()
	settings.TrustScenario = func(m rabbitmq.Message) bool {
		return m.UserID == "orders-api"
	}

	fmt.Println(settings.TrustScenario(rabbitmq.Message{UserID: "orders-api"}))
	fmt.Println(settings.TrustScenario(rabbitmq.Message{})) // published without a user-id
	// Output:
	// true
	// false
}

// relay is a Processor that publishes each order on to the next of the end
// user's services. Each message names the user its Publisher logs in as,
// which RabbitMQ validates, so the next service's TrustScenario can trust
// the scenario the Publisher forwards.
type relay struct {
	pub  *rabbitmq.Publisher // built with ForwardScenario set
	user string              // the user pub logs in as, such as orders-api
}

// Process publishes the order m carries on, with the user-id r.user.
func (r relay) Process(ctx context.Context, m rabbitmq.Message) error {
	return r.pub.Publish(ctx, rabbitmq.Publishing{
		Exchange:   "orders",
		RoutingKey: "accepted",
		MessageID:  m.MessageID,
		UserID:     r.user,
		Body:       m.Body,
	})
}
