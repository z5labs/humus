// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"fmt"

	"github.com/z5labs/humus/queue/rabbitmq"
)

// A Service trusts the failure scenarios of the end user's own services,
// which connect as the user orders-api. RabbitMQ validates a message's
// user-id against the publishing connection's user, so no other publisher
// can claim it. The failure injection page of the docs site quotes this
// example.
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
