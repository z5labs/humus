// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"fmt"

	"github.com/z5labs/humus/queue/kafka"
)

// A Service that consumes a topic only the end user's own services can
// produce to, as the cluster's ACLs decide, trusts the failure scenarios
// its records carry. The failure injection page of the docs site quotes
// this example.
func ExampleSettings_trustScenario() {
	settings := kafka.DefaultSettings()
	settings.TrustScenario = func(m kafka.Message) bool {
		return m.Topic == "orders"
	}

	fmt.Println(settings.TrustScenario(kafka.Message{Topic: "orders"}))
	fmt.Println(settings.TrustScenario(kafka.Message{Topic: "signups"}))
	// Output:
	// true
	// false
}
